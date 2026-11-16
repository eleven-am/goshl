package live

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/ffmpeg"
	"github.com/eleven-am/goshl/internal/mediaexec"
)

var (
	errStall       = errors.New("live source stalled")
	errSourceEnded = errors.New("live source ended")
)

const (
	exitGrace  = 5 * time.Second
	probeGrace = 2 * time.Second
)

type prepared struct {
	reader io.ReadCloser
	cancel context.CancelFunc
	head   []byte
	info   ProbeInfo
	params ffmpeg.LiveParams
	label  string
	slot   bool
}

func (p *prepared) close() {
	p.reader.Close()
	if p.cancel != nil {
		p.cancel()
	}
}

func prepare(ctx context.Context, src Source, opts Options, budget time.Duration) (*prepared, error) {
	reader, err := src.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSourceUnavailable, err)
	}
	if reader == nil {
		return nil, fmt.Errorf("%w: Open returned no reader", ErrSourceUnavailable)
	}
	guard := closeAfter(reader, budget+probeGrace)
	info, head, err := probeReader(ctx, reader, time.Now().Add(budget))
	guard.stop()
	if err == nil && guard.expired() {
		err = ErrNoData
	}
	if err != nil {
		reader.Close()
		return nil, err
	}
	params, err := decide(&info, opts)
	if err != nil {
		reader.Close()
		return nil, err
	}
	return &prepared{reader: reader, head: head, info: info, params: params, label: src.Label}, nil
}

type watchedReader struct {
	reader io.Reader
	last   atomic.Int64
	mu     sync.Mutex
	err    error
}

func (w *watchedReader) Read(buffer []byte) (int, error) {
	n, err := w.reader.Read(buffer)
	if n > 0 {
		w.last.Store(time.Now().UnixNano())
	}
	if err != nil {
		w.mu.Lock()
		w.err = err
		w.mu.Unlock()
	}
	return n, err
}

func (w *watchedReader) failure() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

func (w *watchedReader) idle() time.Duration {
	return time.Since(time.Unix(0, w.last.Load()))
}

type epochResult struct {
	produced int
	err      error
}

func (s *Session) runEpoch(p *prepared) (int, *switchRequest, error) {
	defer p.close()
	s.mu.Lock()
	params := p.params
	params.StartNumber = s.next
	params.TimelineSeconds = s.win.timeline
	params.OutputDir = s.dir
	s.pending = len(s.win.entries) > 0
	s.mu.Unlock()
	args, err := s.manager.config.Builder.Live(params)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %w", ErrUnsupportedCodec, err)
	}
	cmd := exec.CommandContext(s.ctx, "ffmpeg", args...)
	cmd.WaitDelay = exitGrace
	stderr := mediaexec.NewLimitedBuffer(64 << 10)
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 0, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, nil, err
	}
	if err := cmd.Start(); err != nil {
		return 0, nil, mediaexec.Error("ffmpeg", args, "", err)
	}
	source := &watchedReader{reader: p.reader}
	source.last.Store(time.Now().UnixNano())
	pumped := make(chan error, 1)
	go func() {
		_, err := io.Copy(stdin, io.MultiReader(bytes.NewReader(p.head), source))
		stdin.Close()
		pumped <- err
	}()
	finished := make(chan epochResult, 1)
	go func() {
		produced := s.consume(stdout, domain.OutputTimelineOffset.Seconds()+params.TimelineSeconds)
		err := cmd.Wait()
		if err != nil {
			err = mediaexec.Error("ffmpeg", args, stderr.String(), err)
		}
		finished <- epochResult{produced: produced, err: err}
	}()
	stall := time.NewTicker(time.Second)
	defer stall.Stop()
	var request *switchRequest
	var result *epochResult
	var cause error
	pumping := true
	for waiting := true; waiting; {
		select {
		case incoming := <-s.switches:
			request = &incoming
			waiting = false
		case <-pumped:
			pumping = false
			waiting = false
		case done := <-finished:
			result = &done
			waiting = false
		case <-s.ctx.Done():
			waiting = false
		case <-stall.C:
			if source.idle() > s.manager.config.StallTimeout {
				cause = errStall
				waiting = false
			}
		}
	}
	p.reader.Close()
	if pumping {
		<-pumped
	}
	if result == nil {
		timer := time.NewTimer(exitGrace)
		select {
		case done := <-finished:
			result = &done
		case <-timer.C:
			cmd.Process.Kill()
			done := <-finished
			result = &done
		}
		timer.Stop()
	}
	if request != nil || s.ctx.Err() != nil {
		return result.produced, request, nil
	}
	if cause == nil {
		cause = source.failure()
		if errors.Is(cause, io.EOF) {
			cause = errSourceEnded
		}
	}
	if cause == nil && result.err != nil {
		return result.produced, nil, fmt.Errorf("live transcoder failed: %w", result.err)
	}
	if cause == nil {
		cause = errSourceEnded
	}
	return result.produced, nil, fmt.Errorf("%w: %w", ErrSourceUnavailable, cause)
}

func (s *Session) consume(reader io.Reader, base float64) int {
	produced := 0
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		fields := strings.Split(strings.TrimSpace(scanner.Text()), ",")
		if len(fields) < 3 {
			continue
		}
		file := filepath.Base(fields[0])
		sequence, err := sequenceOf(file)
		if err != nil {
			continue
		}
		start, startErr := strconv.ParseFloat(fields[1], 64)
		end, endErr := strconv.ParseFloat(fields[2], 64)
		if startErr != nil || endErr != nil {
			continue
		}
		if s.addSegment(file, sequence, end-max(start, base)) {
			produced++
		}
	}
	return produced
}

func (s *Session) addSegment(file string, sequence int, duration float64) bool {
	s.mu.Lock()
	s.next = max(s.next, sequence+1)
	if duration < 0.01 {
		s.mu.Unlock()
		os.Remove(filepath.Join(s.dir, file))
		return false
	}
	evicted := s.win.add(file, sequence, duration, s.pending, time.Now())
	s.pending = false
	becameReady := s.status == StatusStarting && len(s.win.entries) >= s.opts.ReadySegments
	if becameReady {
		s.status = StatusRunning
		close(s.ready)
	}
	label := s.label
	s.mu.Unlock()
	for _, item := range evicted {
		os.Remove(filepath.Join(s.dir, item.file))
	}
	if becameReady {
		s.manager.observe(EventReady, label, nil)
	}
	return true
}
