package transcode

import (
	"bufio"
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

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/mediaexec"
)

type WorkerState int

const (
	WorkerStateIdle WorkerState = iota
	WorkerStateRunning
	WorkerStateDone
	WorkerStateError
)

type WorkerSpec struct {
	Args      []string
	Storage   domain.Storage
	MediaKey  string
	Rendition string
	IsVideo   bool
	TmpDir    string
	Expected  []int
	Overlap   *int
	Trailing  *int
	Ends      map[int]SegmentEnd
}

type SegmentEnd struct {
	Time  float64
	Early float64
	Late  float64
}

var ErrSegmentTimeline = errors.New("ffmpeg segment does not match the planned time range")

type Worker struct {
	args      []string
	storage   domain.Storage
	mediaKey  string
	rendition string
	isVideo   bool
	tmpDir    string
	expected  map[int]struct{}
	dropped   map[int]struct{}
	ends      map[int]SegmentEnd
	done      chan struct{}

	mu     sync.RWMutex
	state  WorkerState
	err    error
	cmd    *exec.Cmd
	cancel context.CancelFunc
}

var ErrNoExpectedSegments = errors.New("worker requires expected segment indices")

func NewWorker(spec WorkerSpec) *Worker {
	worker := &Worker{
		args:      spec.Args,
		storage:   spec.Storage,
		mediaKey:  spec.MediaKey,
		rendition: spec.Rendition,
		isVideo:   spec.IsVideo,
		tmpDir:    spec.TmpDir,
		expected:  make(map[int]struct{}, len(spec.Expected)),
		dropped:   make(map[int]struct{}, 2),
		ends:      spec.Ends,
		done:      make(chan struct{}),
		state:     WorkerStateIdle,
	}
	for _, index := range spec.Expected {
		worker.expected[index] = struct{}{}
	}
	for _, neighbour := range []*int{spec.Overlap, spec.Trailing} {
		if neighbour != nil {
			worker.dropped[*neighbour] = struct{}{}
		}
	}
	return worker
}

func (w *Worker) Start(ctx context.Context) error {
	if len(w.expected) == 0 {
		return ErrNoExpectedSegments
	}
	w.mu.Lock()
	if w.state != WorkerStateIdle {
		w.mu.Unlock()
		return fmt.Errorf("worker already started")
	}
	ctx, cancel := context.WithCancel(ctx)
	w.state = WorkerStateRunning
	w.cancel = cancel
	w.mu.Unlock()

	cmd, stdout, stderr, err := mediaexec.StartOutput(ctx, "ffmpeg", w.args...)
	if err != nil {
		cancel()
		w.finish(WorkerStateError, err)
		return err
	}
	w.cmd = cmd

	go w.run(ctx, stdout, stderr)

	return nil
}

func (w *Worker) Done() <-chan struct{} {
	return w.done
}

func (w *Worker) run(ctx context.Context, reader io.Reader, stderr fmt.Stringer) {
	uploaded, consumeErr := w.consume(ctx, reader)
	if consumeErr != nil {
		w.Kill()
	}
	cmdErr := w.cmd.Wait()
	if w.cmd.ProcessState != nil && w.cmd.ProcessState.Success() {
		cmdErr = nil
	}

	if consumeErr != nil {
		w.finish(WorkerStateError, consumeErr)
		return
	}
	if cmdErr == nil && w.missingExpected(uploaded) < 0 {
		w.finish(WorkerStateDone, nil)
		return
	}
	if err := ctx.Err(); err != nil {
		w.finish(WorkerStateError, err)
		return
	}
	if cmdErr != nil {
		stderrText := ""
		if stderr != nil {
			stderrText = stderr.String()
		}
		w.finish(WorkerStateError, mediaexec.Error("ffmpeg", w.args, stderrText, cmdErr))
		return
	}
	if len(uploaded) == 0 {
		w.finish(WorkerStateError, fmt.Errorf("ffmpeg produced no segments"))
		return
	}
	if missing := w.missingExpected(uploaded); missing >= 0 {
		w.finish(WorkerStateError, fmt.Errorf("ffmpeg did not produce expected segment %d", missing))
		return
	}
	w.finish(WorkerStateDone, nil)
}

func (w *Worker) consume(ctx context.Context, reader io.Reader) (map[int]struct{}, error) {
	uploaded := make(map[int]struct{})
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return uploaded, err
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		filename := filepath.Base(fields[0])
		index, err := parseSegmentIndex(filename)
		if err != nil {
			return uploaded, err
		}
		if err := w.checkEnd(index, fields); err != nil {
			return uploaded, err
		}
		if _, drop := w.dropped[index]; drop {
			os.Remove(filepath.Join(w.tmpDir, filename))
			continue
		}
		if _, duplicate := uploaded[index]; duplicate {
			return uploaded, fmt.Errorf("duplicate segment %d", index)
		}
		if _, ok := w.expected[index]; !ok {
			return uploaded, fmt.Errorf("ffmpeg produced unexpected segment %d", index)
		}
		if err := w.uploadSegment(ctx, filename, index); err != nil {
			return uploaded, err
		}
		uploaded[index] = struct{}{}
	}
	if err := scanner.Err(); err != nil {
		return uploaded, fmt.Errorf("read ffmpeg output: %w", err)
	}
	return uploaded, nil
}

func (w *Worker) checkEnd(index int, fields []string) error {
	want, ok := w.ends[index]
	if !ok {
		return nil
	}
	if len(fields) < 3 {
		return fmt.Errorf("%w: segment %d has no reported end", ErrSegmentTimeline, index)
	}
	end, err := strconv.ParseFloat(strings.TrimSpace(fields[2]), 64)
	if err != nil {
		return fmt.Errorf("%w: segment %d end %q", ErrSegmentTimeline, index, fields[2])
	}
	if end < want.Time-want.Early || end > want.Time+want.Late {
		return fmt.Errorf("%w: segment %d ends at %.3f, planned %.3f", ErrSegmentTimeline, index, end, want.Time)
	}
	return nil
}

func (w *Worker) missingExpected(uploaded map[int]struct{}) int {
	missing := -1
	for index := range w.expected {
		if _, ok := uploaded[index]; !ok && (missing < 0 || index < missing) {
			missing = index
		}
	}
	return missing
}

func (w *Worker) finish(state WorkerState, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.state = state
	w.err = err
	close(w.done)
}

func (w *Worker) uploadSegment(ctx context.Context, filename string, index int) error {
	filePath := filepath.Join(w.tmpDir, filename)

	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("read segment file %s: %w", filename, err)
	}
	if len(data) == 0 {
		return fmt.Errorf("segment %d is empty", index)
	}

	info := domain.SegmentData{
		MediaKey:  w.mediaKey,
		Index:     index,
		Rendition: w.rendition,
		IsVideo:   w.isVideo,
	}

	if err := w.storage.WriteSegment(ctx, info, data); err != nil {
		return fmt.Errorf("write segment %d: %w", index, err)
	}

	os.Remove(filePath)

	return nil
}

func parseSegmentIndex(filename string) (int, error) {
	name := strings.TrimSuffix(filename, ".ts")
	parts := strings.Split(name, "-")
	if len(parts) < 2 {
		return 0, fmt.Errorf("invalid segment filename: %s", filename)
	}
	index, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		return 0, fmt.Errorf("invalid segment filename: %s", filename)
	}
	return index, nil
}

func (w *Worker) Kill() {
	w.mu.Lock()
	cancel := w.cancel
	w.mu.Unlock()

	if cancel != nil {
		cancel()
	}
}

func (w *Worker) State() WorkerState {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.state
}

func (w *Worker) Err() error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.err
}
