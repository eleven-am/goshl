package goshl

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

func (c *Controller) joinFlight(mediaKey string, streamType domain.StreamType, rendition string, index, endIdx int) (existing *flight, created *flight) {
	r := c.flights
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
			c.releaseFlightLocks(expired)
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
		startBy:    now.Add(c.opts.JobTimeout),
		deadline:   now.Add(2 * c.opts.JobTimeout),
		done:       make(chan struct{}),
	}
	r.flights[f.jobID] = f
	r.mu.Unlock()
	c.releaseFlightLocks(expired)
	return nil, f
}

func (c *Controller) setFlightWatch(f *flight, cancel context.CancelFunc) bool {
	c.flights.mu.Lock()
	defer c.flights.mu.Unlock()
	if c.flights.flights[f.jobID] == f {
		f.watchCancel = cancel
		return true
	}
	cancel()
	return false
}

func (c *Controller) watchFlight(f *flight, statuses <-chan domain.JobStatus) {
	timer := time.NewTimer(time.Until(f.deadline))
	defer timer.Stop()
	for {
		select {
		case <-f.done:
			return
		case <-c.lifecycle.Context().Done():
			c.stopFlight(f)
			return
		case <-timer.C:
			c.expireFlight(f)
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
				c.setFlightTerminal(f, status)
				c.finishFlight(f)
				return
			}
		}
	}
}

func (c *Controller) setFlightLock(f *flight, token string) bool {
	c.flights.mu.Lock()
	defer c.flights.mu.Unlock()
	f.token = token
	return c.flights.flights[f.jobID] == f
}

func (c *Controller) setFlightTerminal(f *flight, status domain.JobStatus) {
	r := c.flights
	r.mu.Lock()
	if r.flights[f.jobID] == f && f.runID == status.RunID {
		f.terminal = status
		f.terminalSet = true
	}
	r.mu.Unlock()
}

func (c *Controller) flightTerminal(f *flight) (domain.JobStatus, bool) {
	c.flights.mu.Lock()
	defer c.flights.mu.Unlock()
	return f.terminal, f.terminalSet
}

func (c *Controller) flightEnded(f *flight) (abandoned, expired bool) {
	c.flights.mu.Lock()
	defer c.flights.mu.Unlock()
	return f.abandoned, f.expired
}

func (c *Controller) abandonFlight(f *flight) {
	c.flights.mu.Lock()
	if c.flights.flights[f.jobID] == f && !f.terminalSet {
		f.abandoned = true
	}
	c.flights.mu.Unlock()
	c.finishFlight(f)
}

func (c *Controller) expireFlight(f *flight) {
	c.flights.mu.Lock()
	if c.flights.flights[f.jobID] == f && !f.terminalSet {
		f.expired = true
	}
	c.flights.mu.Unlock()
	c.finishFlight(f)
}

func (c *Controller) markEnqueued(f *flight) {
	c.flights.mu.Lock()
	f.enqueued = true
	c.flights.mu.Unlock()
}

func (c *Controller) finishFlight(f *flight) {
	c.endFlight(f, true)
}

func (c *Controller) stopFlight(f *flight) {
	c.endFlight(f, false)
}

func (c *Controller) endFlight(f *flight, releaseEnqueued bool) {
	r := c.flights
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
		c.releaseFlightLocks([]*flight{f})
	}
}

func (c *Controller) finishAllFlights() {
	c.flights.mu.Lock()
	pending := make([]*flight, 0, len(c.flights.flights))
	for _, f := range c.flights.flights {
		pending = append(pending, f)
	}
	c.flights.mu.Unlock()
	for _, f := range pending {
		c.stopFlight(f)
	}
}

func (c *Controller) startFlightWatch(f *flight, statuses <-chan domain.JobStatus) bool {
	if !c.lifecycle.Enter() {
		c.stopFlight(f)
		return false
	}
	go func() {
		defer c.lifecycle.Exit()
		c.watchFlight(f, statuses)
	}()
	return true
}

func (c *Controller) finishFlightRun(jobID, runID string) {
	c.flights.mu.Lock()
	f := c.flights.flights[jobID]
	c.flights.mu.Unlock()
	if f != nil && f.runID == runID {
		c.finishFlight(f)
	}
}

func (c *Controller) releaseFlightLocks(flights []*flight) {
	for _, f := range flights {
		c.flights.mu.Lock()
		token := f.token
		c.flights.mu.Unlock()
		if token == "" {
			continue
		}
		cleanup, cancel := cleanupContext()
		if err := c.opts.Coordinator.ReleaseJob(cleanup, f.jobID, f.runID, token); err != nil {
			c.observe(cleanup, domain.Event{Type: domain.EventLockReleaseFailed, MediaKey: f.mediaKey, JobID: f.jobID, Error: err.Error()})
		}
		cancel()
	}
}
