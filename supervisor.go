package suture

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"math/rand"
	"runtime"
	"sync"
	"time"
)

const (
	notRunning = iota
	normal
	paused
	terminated
)

type supervisorID uint32
type serviceID uint32

type (
	// BadStopLogger is called when a service fails to properly stop
	BadStopLogger func(*Supervisor, Service, string)

	// FailureLogger is called when a service fails
	FailureLogger func(
		supervisor *Supervisor,
		service Service,
		serviceName string,
		currentFailures float64,
		failureThreshold float64,
		restarting bool,
		error interface{},
		stacktrace []byte,
	)

	// BackoffLogger is called when the supervisor enters or exits backoff mode
	BackoffLogger func(s *Supervisor, entering bool)
)

var currentSupervisorIDL sync.Mutex
var currentSupervisorID uint32

// ErrWrongSupervisor is returned by the (*Supervisor).Remove method
// if you pass a ServiceToken from the wrong Supervisor.
var ErrWrongSupervisor = errors.New("wrong supervisor for this service token, no service removed")

// ErrTimeout is returned when an attempt to RemoveAndWait for a service to
// stop has timed out.
var ErrTimeout = errors.New("waiting for service to stop has timed out")

// ServiceToken is an opaque identifier that can be used to terminate a service that
// has been Add()ed to a Supervisor.
type ServiceToken struct {
	id uint64
}

type UnstoppedService struct {
	Service      Service
	Name         string
	ServiceToken ServiceToken
}

// An UnstoppedServiceReport will be returned by StopWithReport, reporting
// which services failed to stop.
type UnstoppedServiceReport []UnstoppedService

type serviceWithName struct {
	Service Service
	name    string
}

// Jitter returns the sum of the input duration and a random jitter.  It is
// compatible with the jitter functions in github.com/lthibault/jitterbug.
type Jitter interface {
	Jitter(time.Duration) time.Duration
}

// NoJitter does not apply any jitter to the input duration
type NoJitter struct{}

// Jitter leaves the input duration d unchanged.
func (NoJitter) Jitter(d time.Duration) time.Duration { return d }

// DefaultJitter is the jitter function that is applied when spec.BackoffJitter
// is set to nil.
type DefaultJitter struct {
	rand *rand.Rand
}

// Jitter will jitter the backoff time by uniformly distributing it into
// the range [FailureBackoff, 1.5 * FailureBackoff).
func (dj *DefaultJitter) Jitter(d time.Duration) time.Duration {
	// this is only called by the core supervisor loop, so it is
	// single-thread safe.
	if dj.rand == nil {
		dj.rand = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	jitter := dj.rand.Float64() / 2
	return d + time.Duration(float64(d)*jitter)
}

/*
Supervisor is the core type of the module that represents a Supervisor.

Supervisors should be constructed either by New or NewSimple.

Once constructed, a Supervisor should be started in one of three ways:

 1. Calling .Serve(ctx).
 2. Calling .ServeBackground(ctx).
 3. Adding it to an existing Supervisor.

Calling Serve will cause the supervisor to run until it is shut down by
an external user calling Stop() on it. If that never happens, it simply
runs forever. I suggest creating your services in Supervisors, then making
a Serve() call on your top-level Supervisor be the last line of your main
func.

Calling ServeBackground will CORRECTLY start the supervisor running in a
new goroutine. You do not want to just:

  go supervisor.Serve()

because that will briefly create a race condition as it starts up, if you
try to .Add() services immediately afterward.

The various Log function should only be modified while the Supervisor is
not running, to prevent race conditions.

*/
type Supervisor struct {
	Name string

	spec Spec

	services             map[serviceID]serviceWithName
	cancellations        map[serviceID]context.CancelFunc
	servicesShuttingDown map[serviceID]serviceWithName
	lastFail             time.Time
	failures             float64
	restartQueue         []serviceID
	serviceCounter       serviceID
	control              chan supervisorMessage
	liveness             chan struct{}
	notifyServiceDone    chan serviceID
	resumeTimer          <-chan time.Time

	// This function cancels this supervisor specifically.
	myCancel func()

	// avoid a dependency on github.com/thejerf/abtime by just implementing
	// a minimal chunk.
	getNow       func() time.Time
	getAfterChan func(time.Duration) <-chan time.Time

	sync.Mutex

	// malign leftovers
	id    supervisorID
	state uint8
}

// Spec is used to pass arguments to the New function to create a
// supervisor. See the New function for full documentation.
type Spec struct {
	Log               func(string)
	FailureDecay      float64
	FailureThreshold  float64
	FailureBackoff    time.Duration
	BackoffJitter     Jitter
	Timeout           time.Duration
	LogBadStop        BadStopLogger
	LogFailure        FailureLogger
	LogBackoff        BackoffLogger
	PassThroughPanics bool
}

func (s *Spec) configureDefaults(supervisorName string) {
	if s.FailureDecay == 0 {
		s.FailureDecay = 30
	}
	if s.FailureThreshold == 0 {
		s.FailureThreshold = 5
	}
	if s.FailureBackoff == 0 {
		s.FailureBackoff = time.Second * 15
	}
	if s.BackoffJitter == nil {
		s.BackoffJitter = &DefaultJitter{}
	}
	if s.Timeout == 0 {
		s.Timeout = time.Second * 10
	}

	// set up the default logging handlers
	if s.Log == nil {
		s.Log = func(msg string) {
			log.Print(fmt.Sprintf("Supervisor %s: %s", supervisorName, msg))
		}
	}

	if s.LogBadStop == nil {
		s.LogBadStop = func(sup *Supervisor, _ Service, name string) {
			s.Log(fmt.Sprintf(
				"%s: Service %s failed to terminate in a timely manner",
				sup.Name,
				name,
			))
		}
	}

	if s.LogFailure == nil {
		s.LogFailure = func(
			sup *Supervisor,
			_ Service,
			svcName string,
			f float64,
			thresh float64,
			restarting bool,
			err interface{},
			st []byte,
		) {
			errString := "service returned unexpectedly"
			if err != nil {
				e, canError := err.(error)
				if canError {
					errString = e.Error()
				} else {
					errString = fmt.Sprintf("%#v", err)
				}
			}

			msg := fmt.Sprintf(
				"%s: Failed service '%s' (%f failures of %f), restarting: %#v, error: %s",
				sup.Name,
				svcName,
				f,
				thresh,
				restarting,
				errString,
			)
			if len(st) > 0 {
				msg += fmt.Sprintf(", stacktrace: %s", string(st))
			}

			s.Log(msg)
		}
	}

	if s.LogBackoff == nil {
		s.LogBackoff = func(supervisor *Supervisor, entering bool) {
			if entering {
				s.Log(supervisorName + ": Entering the backoff state.")
			} else {
				s.Log(supervisorName + ": Exiting backoff state.")
			}
		}
	}
}

/*

New is the full constructor function for a supervisor.

The name is a friendly human name for the supervisor, used in logging. Suture
does not care if this is unique, but it is good for your sanity if it is.

If not set, the following values are used:

 * Log:               A function is created that uses log.Print.
 * FailureDecay:      30 seconds
 * FailureThreshold:  5 failures
 * FailureBackoff:    15 seconds
 * Timeout:           10 seconds
 * BackoffJitter:     DefaultJitter

The Log function will be called when errors occur. Suture will log the
following:

 * When a service has failed, with a descriptive message about the
   current backoff status, and whether it was immediately restarted
 * When the supervisor has gone into its backoff mode, and when it
   exits it
 * When a service fails to stop

The failureRate, failureThreshold, and failureBackoff controls how failures
are handled, in order to avoid the supervisor failure case where the
program does nothing but restarting failed services. If you do not
care how failures behave, the default values should be fine for the
vast majority of services, but if you want the details:

The supervisor tracks the number of failures that have occurred, with an
exponential decay on the count. Every FailureDecay seconds, the number of
failures that have occurred is cut in half. (This is done smoothly with an
exponential function.) When a failure occurs, the number of failures
is incremented by one. When the number of failures passes the
FailureThreshold, the entire service waits for FailureBackoff seconds
before attempting any further restarts, at which point it resets its
failure count to zero.

Timeout is how long Suture will wait for a service to properly terminate.

The PassThroughPanics options can be set to let panics in services propagate
and crash the program, should this be desirable.

*/
func New(name string, spec Spec) (s *Supervisor) {
	s = new(Supervisor)

	s.Name = name
	currentSupervisorIDL.Lock()
	currentSupervisorID++
	s.id = supervisorID(currentSupervisorID)
	currentSupervisorIDL.Unlock()

	spec.configureDefaults(s.Name)
	s.spec = spec

	// overriding these allows for testing the threshold behavior
	s.getNow = time.Now
	s.getAfterChan = time.After

	s.control = make(chan supervisorMessage)
	s.liveness = make(chan struct{})
	s.notifyServiceDone = make(chan serviceID)
	s.services = make(map[serviceID]serviceWithName)
	s.cancellations = make(map[serviceID]context.CancelFunc)
	s.servicesShuttingDown = make(map[serviceID]serviceWithName)
	s.restartQueue = make([]serviceID, 0, 1)
	s.resumeTimer = make(chan time.Time)

	return
}

func serviceName(service Service) (serviceName string) {
	stringer, canStringer := service.(fmt.Stringer)
	if canStringer {
		serviceName = stringer.String()
	} else {
		serviceName = fmt.Sprintf("%#v", service)
	}
	return
}

// NewSimple is a convenience function to create a service with just a name
// and the sensible defaults.
func NewSimple(name string) *Supervisor {
	return New(name, Spec{})
}

/*
Add adds a service to this supervisor.

If the supervisor is currently running, the service will be started
immediately. If the supervisor is not currently running, the service
will be started when the supervisor is.

The returned ServiceID may be passed to the Remove method of the Supervisor
to terminate the service.

As a special behavior, if the service added is itself a supervisor, the
supervisor being added will copy the Log function from the Supervisor it
is being added to. This allows factoring out providing a Supervisor
from its logging. This unconditionally overwrites the child Supervisor's
logging functions.

*/
func (s *Supervisor) Add(service Service) ServiceToken {
	if s == nil {
		panic("can't add service to nil *suture.Supervisor")
	}

	if supervisor, isSupervisor := service.(*Supervisor); isSupervisor {
		supervisor.spec.LogBadStop = s.spec.LogBadStop
		supervisor.spec.LogFailure = s.spec.LogFailure
		supervisor.spec.LogBackoff = s.spec.LogBackoff
	}

	s.Lock()
	if s.state == notRunning {
		id := s.serviceCounter
		s.serviceCounter++

		s.services[id] = serviceWithName{service, serviceName(service)}
		s.restartQueue = append(s.restartQueue, id)

		s.Unlock()
		return ServiceToken{uint64(s.id)<<32 | uint64(id)}
	}
	s.Unlock()

	response := make(chan serviceID)
	s.control <- addService{service, serviceName(service), response}
	return ServiceToken{uint64(s.id)<<32 | uint64(<-response)}
}

// ServeBackground starts running a supervisor in its own goroutine. When
// this method returns, the supervisor is guaranteed to be in a running state.
func (s *Supervisor) ServeBackground(ctx context.Context) {
	go s.Serve(ctx)
	s.sync()
}

/*
Serve starts the supervisor. You should call this on the top-level supervisor,
but nothing else.
*/
func (s *Supervisor) Serve(ctx context.Context) error {
	// context documentation suggests that it is legal for functions to
	// take nil contexts, it's users that shouldn't ever pass them.
	if ctx == nil {
		ctx = context.Background()
	}

	ctx, myCancel := context.WithCancel(ctx)
	s.myCancel = myCancel

	if s == nil {
		panic("Can't serve with a nil *suture.Supervisor")
	}
	if s.id == 0 {
		panic("Can't call Serve on an incorrectly-constructed *suture.Supervisor")
	}

	s.Lock()
	if s.state == terminated {
		// Got stopped before we got started.
		s.Unlock()
		return nil
	}

	if s.state != notRunning {
		s.Unlock()
		panic("Called .Serve() on a supervisor that is already Serve()ing")
	}

	s.state = normal
	s.Unlock()

	defer func() {
		s.Lock()
		s.state = notRunning
		s.Unlock()
	}()

	// for all the services I currently know about, start them
	for _, id := range s.restartQueue {
		namedService, present := s.services[id]
		if present {
			s.runService(ctx, namedService.Service, id)
		}
	}
	s.restartQueue = make([]serviceID, 0, 1)

	for {
		select {
		case <-ctx.Done():
			s.stopSupervisor()
			return ctx.Err()
		case m := <-s.control:
			switch msg := m.(type) {
			case serviceFailed:
				s.handleFailedService(ctx, msg.id, msg.err, msg.stacktrace)
			case serviceEnded:
				_, monitored := s.services[msg.id]
				if monitored {
					cancel := s.cancellations[msg.id]
					if errors.Is(msg.err, ErrComplete) {
						delete(s.services, msg.id)
						delete(s.cancellations, msg.id)
						go func() {
							cancel()
						}()
					} else if errors.Is(msg.err, ErrAbort) {
						s.stopSupervisor()
						return msg.err
					} else {
						s.handleFailedService(ctx, msg.id, msg.err, nil)
					}
				}
			case addService:
				id := s.serviceCounter
				s.serviceCounter++

				s.services[id] = serviceWithName{msg.service, msg.name}
				s.runService(ctx, msg.service, id)

				msg.response <- id
			case removeService:
				s.removeService(msg.id, msg.notification)
			case stopSupervisor:
				msg.done <- s.stopSupervisor()
				return nil
			case listServices:
				services := []Service{}
				for _, service := range s.services {
					services = append(services, service.Service)
				}
				msg.c <- services
			case syncSupervisor:
				// this does nothing on purpose; its sole purpose is to
				// introduce a sync point via the channel receive
			case panicSupervisor:
				// used only by tests
				panic("Panicking as requested!")
			}
		case serviceEnded := <-s.notifyServiceDone:
			delete(s.servicesShuttingDown, serviceEnded)
		case <-s.resumeTimer:
			// We're resuming normal operation after a pause due to
			// excessive thrashing
			// FIXME: Ought to permit some spacing of these functions, rather
			// than simply hammering through them
			s.Lock()
			s.state = normal
			s.Unlock()
			s.failures = 0
			s.spec.LogBackoff(s, false)
			for _, id := range s.restartQueue {
				namedService, present := s.services[id]
				if present {
					s.runService(ctx, namedService.Service, id)
				}
			}
			s.restartQueue = make([]serviceID, 0, 1)
		}
	}
}

// StopWithReport will stop the supervisor like calling Stop, but will also
// return a struct reporting what services failed to stop. This fully
// encompasses calling Stop, so do not call Stop and StopWithReport any
// more than you should call Stop twice.
//
// WARNING: Technically, any use of the returned data structure is a
// TOCTOU violation:
// https://en.wikipedia.org/wiki/Time-of-check_to_time-of-use
// Since the data structure was generated and returned to you, any of these
// services may have stopped since then.
//
// However, this can still be useful information at program teardown
// time. For instance, logging that a service failed to stop as expected is
// still useful, as even if it shuts down later, it was still later than
// you expected.
//
// But if you cast the Service objects back to their underlying objects and
// start trying to manipulate them ("shut down harder!"), be sure to
// account for the possibility they are in fact shut down before you get
// them.
//
// If there are no services to report, the UnstoppedServiceReport will be
// nil. A zero-length constructed slice is never returned.
//
// Calling this on an already-stopped supervisor is invalid, but will
// safely return nil anyhow.
func (s *Supervisor) StopWithReport() UnstoppedServiceReport {
	s.Lock()
	if s.state == notRunning {
		s.state = terminated
		s.Unlock()
		return nil
	}
	s.state = terminated
	s.Unlock()

	done := make(chan UnstoppedServiceReport)
	if s.sendControl(stopSupervisor{done}) {
		return <-done
	}
	return nil
}

// Stop stops the Supervisor.
//
// This function will not return until either all Services have stopped, or
// they timeout after the timeout value given to the Supervisor at
// creation.
func (s *Supervisor) Stop() {
	s.StopWithReport()
}

func (s *Supervisor) handleFailedService(ctx context.Context, id serviceID, err interface{}, stacktrace []byte) {
	now := s.getNow()

	if s.lastFail.IsZero() {
		s.lastFail = now
		s.failures = 1.0
	} else {
		sinceLastFail := now.Sub(s.lastFail).Seconds()
		intervals := sinceLastFail / s.spec.FailureDecay
		s.failures = s.failures*math.Pow(.5, intervals) + 1
	}

	if s.failures > s.spec.FailureThreshold {
		s.Lock()
		s.state = paused
		s.Unlock()
		s.spec.LogBackoff(s, true)
		s.resumeTimer = s.getAfterChan(
			s.spec.BackoffJitter.Jitter(s.spec.FailureBackoff))
	}

	s.lastFail = now

	failedService, monitored := s.services[id]

	// It is possible for a service to be no longer monitored
	// by the time we get here. In that case, just ignore it.
	if monitored {
		// this may look dangerous because the state could change, but this
		// code is only ever run in the one goroutine that is permitted to
		// change the state, so nothing else will.
		s.Lock()
		curState := s.state
		s.Unlock()
		if curState == normal {
			s.runService(ctx, failedService.Service, id)
			s.spec.LogFailure(
				s,
				failedService.Service,
				failedService.name,
				s.failures,
				s.spec.FailureThreshold,
				true,
				err,
				stacktrace,
			)
		} else {
			// FIXME: When restarting, check that the service still
			// exists (it may have been stopped in the meantime)
			s.restartQueue = append(s.restartQueue, id)
			s.spec.LogFailure(
				s,
				failedService.Service,
				failedService.name,
				s.failures,
				s.spec.FailureThreshold,
				false,
				err,
				stacktrace,
			)
		}
	}
}

func (s *Supervisor) runService(ctx context.Context, service Service, id serviceID) {
	childCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	blockingCancellation := func() {
		cancel()
		<-done
	}
	s.cancellations[id] = blockingCancellation
	go func() {
		if !s.spec.PassThroughPanics {
			defer func() {
				if r := recover(); r != nil {
					buf := make([]byte, 65535)
					written := runtime.Stack(buf, false)
					buf = buf[:written]
					s.fail(id, r, buf)
				}
			}()
		}

		err := service.Serve(childCtx)
		cancel()
		close(done)

		s.serviceEnded(id, err)
	}()
}

func (s *Supervisor) removeService(id serviceID, notificationChan chan struct{}) {
	namedService, present := s.services[id]
	if present {
		cancel := s.cancellations[id]
		delete(s.services, id)
		delete(s.cancellations, id)

		s.servicesShuttingDown[id] = namedService
		go func() {
			successChan := make(chan struct{})
			go func() {
				cancel()
				close(successChan)
				if notificationChan != nil {
					notificationChan <- struct{}{}
				}
			}()

			select {
			case <-successChan:
				// Life is good!
			case <-s.getAfterChan(s.spec.Timeout):
				s.spec.LogBadStop(
					s,
					namedService.Service,
					namedService.name,
				)
			}
			s.notifyServiceDone <- id
		}()
	} else {
		if notificationChan != nil {
			notificationChan <- struct{}{}
		}
	}
}

func (s *Supervisor) stopSupervisor() UnstoppedServiceReport {
	notifyDone := make(chan serviceID, len(s.services))

	for id := range s.services {
		namedService, present := s.services[id]
		if present {
			cancel := s.cancellations[id]
			delete(s.services, id)
			delete(s.cancellations, id)
			s.servicesShuttingDown[id] = namedService
			go func(sID serviceID) {
				cancel()
				notifyDone <- sID
			}(id)
		}
	}

	timeout := s.getAfterChan(s.spec.Timeout)
SHUTTING_DOWN_SERVICES:
	for len(s.servicesShuttingDown) > 0 {
		select {
		case id := <-notifyDone:
			delete(s.servicesShuttingDown, id)
		case serviceID := <-s.notifyServiceDone:
			delete(s.servicesShuttingDown, serviceID)
		case <-timeout:
			for _, namedService := range s.servicesShuttingDown {
				s.spec.LogBadStop(
					s,
					namedService.Service,
					namedService.name,
				)
			}

			// failed remove statements will log the errors.
			break SHUTTING_DOWN_SERVICES
		}
	}

	close(s.liveness)

	if len(s.servicesShuttingDown) == 0 {
		return nil
	} else {
		report := UnstoppedServiceReport{}
		for serviceID, serviceWithName := range s.servicesShuttingDown {
			report = append(report, UnstoppedService{
				Service:      serviceWithName.Service,
				Name:         serviceWithName.name,
				ServiceToken: ServiceToken{uint64(s.id)<<32 | uint64(serviceID)},
			})
		}
		return report
	}
}

// String implements the fmt.Stringer interface.
func (s *Supervisor) String() string {
	return s.Name
}

// sendControl abstracts checking for the supervisor to still be running
// when we send a message. This way, if someone does call Stop twice on a
// supervisor or call stop in one goroutine while calling Stop in another,
// the goroutines trying to call methods on a stopped supervisor won't hang
// forever and leak.
func (s *Supervisor) sendControl(sm supervisorMessage) bool {
	select {
	case s.control <- sm:
		return true
	case <-s.liveness:
		return false
	}
}

/*
Remove will remove the given service from the Supervisor, and attempt to Stop() it.
The ServiceID token comes from the Add() call. This returns without waiting
for the service to stop.
*/
func (s *Supervisor) Remove(id ServiceToken) error {
	sID := supervisorID(id.id >> 32)
	if sID != s.id {
		return ErrWrongSupervisor
	}
	// no meaningful error handling if this is false
	_ = s.sendControl(removeService{serviceID(id.id & 0xffffffff), nil})
	return nil
}

/*
RemoveAndWait will remove the given service from the Supervisor and attempt
to Stop() it. It will wait up to the given timeout value for the service to
terminate. A timeout value of 0 means to wait forever.

If a nil error is returned from this function, then the service was
terminated normally. If either the supervisor terminates or the timeout
passes, ErrTimeout is returned. (If this isn't even the right supervisor
ErrWrongSupervisor is returned.)
*/
func (s *Supervisor) RemoveAndWait(id ServiceToken, timeout time.Duration) error {
	sID := supervisorID(id.id >> 32)
	if sID != s.id {
		return ErrWrongSupervisor
	}

	var timeoutC <-chan time.Time

	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		timeoutC = timer.C
	}

	notificationC := make(chan struct{})

	sentControl := s.sendControl(removeService{serviceID(id.id & 0xffffffff), notificationC})

	if !sentControl {
		return ErrTimeout
	}

	select {
	case <-notificationC:
		// normal case; the service is terminated.
		return nil

	// This occurs if the entire supervisor ends without the service
	// having terminated, and includes the timeout the supervisor
	// itself waited before closing the liveness channel.
	case <-s.liveness:
		return ErrTimeout

	// The local timeout.
	case <-timeoutC:
		return ErrTimeout
	}
}

/*

Services returns a []Service containing a snapshot of the services this
Supervisor is managing.

*/
func (s *Supervisor) Services() []Service {
	ls := listServices{make(chan []Service)}

	if s.sendControl(ls) {
		return <-ls.c
	}
	return nil
}
