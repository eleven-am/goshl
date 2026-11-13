package engine

import (
	"context"
	"sync"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/google/uuid"
)

type flight struct {
	jobID       string
	runID       string
	mediaKey    string
	streamType  domain.StreamType
	rendition   string
	start       int
	end         int
	startBy     time.Time
	deadline    time.Time
	token       string
	watchCancel context.CancelFunc
	done        chan struct{}
	terminal    domain.JobStatus
	terminalSet bool
	abandoned   bool
	expired     bool
	enqueued    bool
}

type flightRegistry struct {
	mu      sync.Mutex
	flights map[string]*flight
}

func newFlightRegistry() *flightRegistry {
	return &flightRegistry{flights: make(map[string]*flight)}
}

func (f *flight) sameStream(mediaKey string, streamType domain.StreamType, rendition string) bool {
	return f.mediaKey == mediaKey && f.streamType == streamType && f.rendition == rendition
}

func (e *Engine) joinFlight(mediaKey string, streamType domain.StreamType, rendition string, index, endIdx int) (existing *flight, created *flight) {
	r := e.flights
	now := time.Now()
	r.mu.Lock()
	var expired []*flight
	for id, f := range r.flights {
		if now.After(f.deadline) {
			delete(r.flights, id)
			f.expired = true
			if f.watchCancel != nil {
				f.watchCancel()
			}
			close(f.done)
			expired = append(expired, f)
		}
	}
	for _, f := range r.flights {
		if f.sameStream(mediaKey, streamType, rendition) && index >= f.start && index <= f.end {
			r.mu.Unlock()
			e.releaseFlightLocks(expired)
			return f, nil
		}
	}
	end := endIdx
	for _, f := range r.flights {
		if f.sameStream(mediaKey, streamType, rendition) && f.start > index && f.start-1 < end {
			end = f.start - 1
		}
	}
	f := &flight{
		jobID:      deterministicJobID(mediaKey, rendition, streamType, index, end),
		runID:      uuid.NewString(),
		mediaKey:   mediaKey,
		streamType: streamType,
		rendition:  rendition,
		start:      index,
		end:        end,
		startBy:    now.Add(e.opts.JobTimeout),
		deadline:   now.Add(2 * e.opts.JobTimeout),
		done:       make(chan struct{}),
	}
	r.flights[f.jobID] = f
	r.mu.Unlock()
	e.releaseFlightLocks(expired)
	return nil, f
}

func (e *Engine) setFlightWatch(f *flight, cancel context.CancelFunc) bool {
	e.flights.mu.Lock()
	defer e.flights.mu.Unlock()
	if e.flights.flights[f.jobID] == f {
		f.watchCancel = cancel
		return true
	}
	cancel()
	return false
}

func (e *Engine) watchFlight(f *flight, statuses <-chan domain.JobStatus) {
	timer := time.NewTimer(time.Until(f.deadline))
	defer timer.Stop()
	for {
		select {
		case <-f.done:
			return
		case <-e.lifecycle.Context().Done():
			e.stopFlight(f)
			return
		case <-timer.C:
			e.expireFlight(f)
			return
		case status, ok := <-statuses:
			if !ok {
				statuses = nil
				continue
			}
			if status.RunID != f.runID {
				continue
			}
			switch status.State {
			case domain.JobStateCompleted, domain.JobStateFailed, domain.JobStateCancelled:
				e.setFlightTerminal(f, status)
				e.finishFlight(f)
				return
			}
		}
	}
}

func (e *Engine) setFlightLock(f *flight, token string) bool {
	e.flights.mu.Lock()
	defer e.flights.mu.Unlock()
	f.token = token
	return e.flights.flights[f.jobID] == f
}

func (e *Engine) setFlightTerminal(f *flight, status domain.JobStatus) {
	r := e.flights
	r.mu.Lock()
	if r.flights[f.jobID] == f && f.runID == status.RunID {
		f.terminal = status
		f.terminalSet = true
	}
	r.mu.Unlock()
}

func (e *Engine) flightTerminal(f *flight) (domain.JobStatus, bool) {
	e.flights.mu.Lock()
	defer e.flights.mu.Unlock()
	return f.terminal, f.terminalSet
}

func (e *Engine) flightEnded(f *flight) (abandoned, expired bool) {
	e.flights.mu.Lock()
	defer e.flights.mu.Unlock()
	return f.abandoned, f.expired
}

func (e *Engine) abandonFlight(f *flight) {
	e.flights.mu.Lock()
	if e.flights.flights[f.jobID] == f && !f.terminalSet {
		f.abandoned = true
	}
	e.flights.mu.Unlock()
	e.finishFlight(f)
}

func (e *Engine) expireFlight(f *flight) {
	e.flights.mu.Lock()
	if e.flights.flights[f.jobID] == f && !f.terminalSet {
		f.expired = true
	}
	e.flights.mu.Unlock()
	e.finishFlight(f)
}

func (e *Engine) markEnqueued(f *flight) bool {
	e.flights.mu.Lock()
	defer e.flights.mu.Unlock()
	if e.flights.flights[f.jobID] != f {
		return false
	}
	f.enqueued = true
	return true
}

const lockProbeTTL = 5 * time.Second

func (e *Engine) jobLockFree(jobID string) bool {
	probe, cancel := cleanupContext()
	defer cancel()
	runID := uuid.NewString()
	token, _, err := e.opts.Coordinator.AcquireJob(probe, jobID, runID, lockProbeTTL)
	if err != nil {
		return false
	}
	if err := e.opts.Coordinator.ReleaseJob(probe, jobID, runID, token); err != nil {
		e.observe(probe, domain.Event{Type: domain.EventLockReleaseFailed, JobID: jobID, Error: err.Error()})
	}
	return true
}

func (e *Engine) finishFlight(f *flight) {
	e.endFlight(f, true)
}

func (e *Engine) stopFlight(f *flight) {
	e.endFlight(f, false)
}

func (e *Engine) endFlight(f *flight, releaseEnqueued bool) {
	r := e.flights
	r.mu.Lock()
	current, ok := r.flights[f.jobID]
	ok = ok && current == f && current.runID == f.runID
	release := ok && (releaseEnqueued || !f.enqueued)
	if ok {
		delete(r.flights, f.jobID)
		if f.watchCancel != nil {
			f.watchCancel()
		}
		close(f.done)
	}
	r.mu.Unlock()
	if release {
		e.releaseFlightLocks([]*flight{f})
	}
}

func (e *Engine) finishAllFlights() {
	e.flights.mu.Lock()
	pending := make([]*flight, 0, len(e.flights.flights))
	for _, f := range e.flights.flights {
		pending = append(pending, f)
	}
	e.flights.mu.Unlock()
	for _, f := range pending {
		e.stopFlight(f)
	}
}

func (e *Engine) startFlightWatch(f *flight, statuses <-chan domain.JobStatus) bool {
	if !e.lifecycle.Enter() {
		e.stopFlight(f)
		return false
	}
	go func() {
		defer e.lifecycle.Exit()
		e.watchFlight(f, statuses)
	}()
	return true
}

func (e *Engine) releaseFlightLocks(flights []*flight) {
	for _, f := range flights {
		e.flights.mu.Lock()
		token := f.token
		e.flights.mu.Unlock()
		if token == "" {
			continue
		}
		cleanup, cancel := cleanupContext()
		if err := e.opts.Coordinator.ReleaseJob(cleanup, f.jobID, f.runID, token); err != nil {
			e.observe(cleanup, domain.Event{Type: domain.EventLockReleaseFailed, MediaKey: f.mediaKey, JobID: f.jobID, Error: err.Error()})
		}
		cancel()
	}
}
