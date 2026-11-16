package livetest

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/eleven-am/goshl/internal/live"
)

const packetSize = 188

var ErrInjected = errors.New("injected reader failure")

type Control struct {
	mu      sync.Mutex
	opens   int
	refuse  int
	current *paced
	Loop    bool
	Burst   time.Duration
	Unpaced bool
}

func (c *Control) Opens() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.opens
}

func (c *Control) RefuseOpens(count int) {
	c.mu.Lock()
	c.refuse = count
	c.mu.Unlock()
}

func (c *Control) Fail() {
	c.mu.Lock()
	current := c.current
	c.mu.Unlock()
	if current != nil {
		current.inject(ErrInjected)
	}
}

func (c *Control) Stall() {
	c.mu.Lock()
	current := c.current
	c.mu.Unlock()
	if current != nil {
		current.stall()
	}
}

func FileSource(path, label string) (live.Source, *Control) {
	control := &Control{Loop: true, Burst: 2 * time.Second}
	source := live.Source{Label: label, Open: func(ctx context.Context) (io.ReadCloser, error) {
		control.mu.Lock()
		defer control.mu.Unlock()
		control.opens++
		if control.refuse > 0 {
			control.refuse--
			return nil, ErrInjected
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		reader := &paced{file: file, loop: control.Loop, burst: control.Burst, unpaced: control.Unpaced, closed: make(chan struct{}), base: -1}
		control.current = reader
		return reader, nil
	}}
	return source, control
}

type paced struct {
	file    *os.File
	loop    bool
	burst   time.Duration
	unpaced bool
	closed  chan struct{}
	once    sync.Once
	base    int64
	origin  time.Time

	mu      sync.Mutex
	failure error
	stalled bool
}

func (p *paced) inject(err error) {
	p.mu.Lock()
	p.failure = err
	p.mu.Unlock()
}

func (p *paced) stall() {
	p.mu.Lock()
	p.stalled = true
	p.mu.Unlock()
}

func (p *paced) Close() error {
	p.once.Do(func() {
		close(p.closed)
		p.file.Close()
	})
	return nil
}

func (p *paced) Read(buffer []byte) (int, error) {
	p.mu.Lock()
	failure, stalled := p.failure, p.stalled
	p.mu.Unlock()
	if failure != nil {
		return 0, failure
	}
	if stalled {
		<-p.closed
		return 0, os.ErrClosed
	}
	select {
	case <-p.closed:
		return 0, os.ErrClosed
	default:
	}
	if len(buffer) < packetSize {
		return 0, io.ErrShortBuffer
	}
	packets := min(len(buffer)/packetSize, 64)
	total := 0
	for index := 0; index < packets; index++ {
		packet := buffer[total : total+packetSize]
		if _, err := io.ReadFull(p.file, packet); err != nil {
			if !p.loop {
				if total > 0 {
					return total, nil
				}
				return 0, io.EOF
			}
			if _, err := p.file.Seek(0, io.SeekStart); err != nil {
				return total, err
			}
			p.base = -1
			p.burst = 0
			if total > 0 {
				return total, nil
			}
			index--
			continue
		}
		total += packetSize
		if clock, ok := pcr(packet); ok && !p.unpaced {
			if p.base < 0 {
				p.base = clock
				p.origin = time.Now().Add(-p.burst)
			}
			due := p.origin.Add(time.Duration(clock-p.base) * time.Second / 90000)
			if wait := time.Until(due); wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-timer.C:
				case <-p.closed:
					timer.Stop()
					return 0, os.ErrClosed
				}
				return total, nil
			}
		}
	}
	return total, nil
}

func pcr(packet []byte) (int64, bool) {
	if packet[0] != 0x47 || packet[3]&0x20 == 0 || packet[4] < 7 || packet[5]&0x10 == 0 {
		return 0, false
	}
	base := int64(packet[6])<<25 | int64(packet[7])<<17 | int64(packet[8])<<9 | int64(packet[9])<<1 | int64(packet[10])>>7
	return base, true
}
