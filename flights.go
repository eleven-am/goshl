package goshl

import (
	"context"
	"sync"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
)

type jobPool interface {
	CancelJob(jobID string)
	ReviveJob(jobID string)
}

type flight struct {
	jobID      string
	mediaKey   string
	streamType domain.StreamType
	rendition  string
	start      int
	end        int
	deadline   time.Time
	lockKey    string
	token      string
	refs       int
	timer      *time.Timer
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

func (c *Controller) poolFor(streamType domain.StreamType) jobPool {
	if streamType == domain.StreamVideo {
		return c.videoPool
	}
	return c.audioPool
}

func (c *Controller) joinFlight(mediaKey string, streamType domain.StreamType, rendition string, index, endIdx int) (existing *flight, created *flight) {
	r := c.flights
	now := time.Now()
	r.mu.Lock()
	var expired []*flight
	for id, f := range r.flights {
		if now.After(f.deadline) {
			delete(r.flights, id)
			if f.timer != nil {
				f.timer.Stop()
			}
			expired = append(expired, f)
		}
	}
	for _, f := range r.flights {
		if f.sameStream(mediaKey, streamType, rendition) && index >= f.start && index <= f.end {
			f.refs++
			if f.timer != nil {
				f.timer.Stop()
				f.timer = nil
			}
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
		mediaKey:   mediaKey,
		streamType: streamType,
		rendition:  rendition,
		start:      index,
		end:        end,
		deadline:   now.Add(c.opts.JobTimeout),
		refs:       1,
	}
	r.flights[f.jobID] = f
	r.mu.Unlock()
	c.releaseFlightLocks(expired)
	return nil, f
}

func (c *Controller) setFlightLock(f *flight, lockKey, token string) {
	c.flights.mu.Lock()
	f.lockKey = lockKey
	f.token = token
	c.flights.mu.Unlock()
}

func (c *Controller) leaveFlight(f *flight) {
	r := c.flights
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.flights[f.jobID] != f {
		return
	}
	f.refs--
	if f.refs > 0 || f.timer != nil {
		return
	}
	f.timer = time.AfterFunc(c.opts.AbandonGrace, func() { c.abandonFlight(f) })
}

func (c *Controller) abandonFlight(f *flight) {
	r := c.flights
	r.mu.Lock()
	if r.flights[f.jobID] != f || f.refs > 0 {
		r.mu.Unlock()
		return
	}
	f.timer = nil
	r.mu.Unlock()
	c.poolFor(f.streamType).CancelJob(f.jobID)
	c.observe(context.Background(), domain.Event{Type: domain.EventJobAbandoned, MediaKey: f.mediaKey, JobID: f.jobID, StreamType: f.streamType, Rendition: f.rendition, StartIndex: f.start, EndIndex: f.end})
}

func (c *Controller) finishFlight(jobID string) {
	r := c.flights
	r.mu.Lock()
	f, ok := r.flights[jobID]
	if ok {
		delete(r.flights, jobID)
		if f.timer != nil {
			f.timer.Stop()
		}
	}
	r.mu.Unlock()
	if !ok {
		return
	}
	c.poolFor(f.streamType).ReviveJob(jobID)
	c.releaseFlightLocks([]*flight{f})
}

func (c *Controller) releaseFlightLocks(flights []*flight) {
	for _, f := range flights {
		c.flights.mu.Lock()
		lockKey, token := f.lockKey, f.token
		c.flights.mu.Unlock()
		if lockKey == "" {
			continue
		}
		cleanup, cancel := cleanupContext()
		if err := c.opts.Coordinator.ReleaseLock(cleanup, lockKey, token); err != nil {
			c.observe(cleanup, domain.Event{Type: domain.EventLockReleaseFailed, MediaKey: f.mediaKey, JobID: f.jobID, Error: err.Error()})
		}
		cancel()
	}
}
