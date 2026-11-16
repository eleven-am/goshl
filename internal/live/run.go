package live

import (
	"errors"
	"time"

	"github.com/cenkalti/backoff/v5"
)

type reconnector struct {
	delays   *backoff.ExponentialBackOff
	attempts int
	limit    int
}

func newReconnector(initial, ceiling time.Duration, limit int) *reconnector {
	delays := backoff.NewExponentialBackOff()
	delays.InitialInterval = initial
	delays.MaxInterval = ceiling
	delays.RandomizationFactor = 0
	delays.Multiplier = 2
	delays.Reset()
	return &reconnector{delays: delays, limit: limit}
}

func (r *reconnector) reset() {
	r.attempts = 0
	r.delays.Reset()
}

func (r *reconnector) next(floor time.Duration) (time.Duration, bool) {
	if r.attempts >= r.limit {
		return 0, false
	}
	r.attempts++
	if r.attempts == 1 {
		return floor, true
	}
	return max(r.delays.NextBackOff(), floor), true
}

func permanent(err error) bool {
	return errors.Is(err, ErrUnsupportedCodec) || errors.Is(err, ErrCapacity)
}

func (s *Session) pause(delay time.Duration) (*switchRequest, bool) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil, true
	case request := <-s.switches:
		return &request, true
	case <-s.ctx.Done():
		return nil, false
	}
}

func (s *Session) accept(request *switchRequest, retry *reconnector) (Source, *prepared) {
	retry.reset()
	s.manager.observe(EventSwitched, request.src.Label, nil)
	return request.src, request.prepared
}

func (s *Session) run(src Source) {
	defer s.finish()
	s.manager.observe(EventStarted, src.Label, nil)
	go s.watchIdle()
	config := s.manager.config
	retry := newReconnector(config.ReconnectInitial, config.ReconnectMax, config.ReconnectAttempts)
	var next *prepared
	started := false
	for {
		if next == nil {
			candidate, err := s.prepareSource(s.ctx, src)
			if err != nil {
				if s.ctx.Err() != nil {
					return
				}
				delay, again := retry.next(config.ReconnectInitial)
				if !started || permanent(err) || !again {
					s.fail(err)
					return
				}
				s.manager.observe(EventReconnecting, src.Label, err)
				request, alive := s.pause(delay)
				if !alive {
					return
				}
				if request != nil {
					src, next = s.accept(request, retry)
				}
				continue
			}
			next = candidate
		}
		if err := s.adopt(next); err != nil {
			s.discard(next)
			s.fail(err)
			return
		}
		started = true
		produced, request, err := s.runEpoch(next)
		next = nil
		if s.ctx.Err() != nil {
			if request != nil {
				s.discard(request.prepared)
			}
			return
		}
		if request != nil {
			src, next = s.accept(request, retry)
			continue
		}
		if permanent(err) {
			s.fail(err)
			return
		}
		if produced > 0 {
			retry.reset()
		}
		delay, again := retry.next(0)
		if !again {
			s.fail(err)
			return
		}
		s.manager.observe(EventReconnecting, src.Label, err)
		request, alive := s.pause(delay)
		if !alive {
			return
		}
		if request != nil {
			src, next = s.accept(request, retry)
		}
	}
}
