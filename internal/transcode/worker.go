package transcode

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
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

type Worker struct {
	args      []string
	storage   domain.Storage
	mediaKey  string
	rendition string
	isVideo   bool
	tmpDir    string
	skipFirst bool
	expected  map[int]struct{}

	mu     sync.RWMutex
	state  WorkerState
	err    error
	cmd    interface{ Wait() error }
	cancel context.CancelFunc
}

func NewWorker(args []string, storage domain.Storage, mediaKey string, rendition string, isVideo bool, tmpDir string, skipFirst bool, expectedIndices ...[]int) *Worker {
	worker := &Worker{
		args:      args,
		storage:   storage,
		mediaKey:  mediaKey,
		rendition: rendition,
		isVideo:   isVideo,
		tmpDir:    tmpDir,
		skipFirst: skipFirst,
		state:     WorkerStateIdle,
	}
	if len(expectedIndices) > 0 {
		worker.expected = make(map[int]struct{}, len(expectedIndices[0]))
		for _, index := range expectedIndices[0] {
			worker.expected[index] = struct{}{}
		}
	}
	return worker
}

func (w *Worker) Start(ctx context.Context) error {
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
		w.setError(err)
		return err
	}
	w.cmd = cmd

	go w.run(ctx, stdout, stderr)

	return nil
}

func (w *Worker) run(ctx context.Context, reader io.Reader, stderr fmt.Stringer) {
	scanner := bufio.NewScanner(reader)
	skipFirst := w.skipFirst
	uploadedSegments := 0
	uploaded := make(map[int]struct{})

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			_ = w.cmd.Wait()
			w.setError(ctx.Err())
			return
		default:
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		filename := filepath.Base(line)

		if skipFirst {
			skipFirst = false
			os.Remove(filepath.Join(w.tmpDir, filename))
			continue
		}

		index, err := parseSegmentIndex(filename)
		if err != nil {
			w.setError(err)
			w.Kill()
			_ = w.cmd.Wait()
			return
		}
		if _, duplicate := uploaded[index]; duplicate {
			w.Kill()
			_ = w.cmd.Wait()
			w.setError(fmt.Errorf("duplicate segment %d", index))
			return
		}
		if len(w.expected) > 0 {
			if _, ok := w.expected[index]; !ok {
				w.Kill()
				_ = w.cmd.Wait()
				w.setError(fmt.Errorf("ffmpeg produced unexpected segment %d", index))
				return
			}
		}
		err = w.uploadSegment(ctx, filename, index)
		if err != nil {
			w.setError(err)
			w.Kill()
			_ = w.cmd.Wait()
			return
		}
		uploaded[index] = struct{}{}
		uploadedSegments++
	}

	if err := scanner.Err(); err != nil {
		_ = w.cmd.Wait()
		w.setError(fmt.Errorf("read ffmpeg output: %w", err))
		return
	}

	cmdErr := w.cmd.Wait()

	w.mu.Lock()
	defer w.mu.Unlock()
	if ctx.Err() != nil {
		w.state = WorkerStateError
		w.err = ctx.Err()
		return
	}

	if cmdErr != nil {
		w.state = WorkerStateError
		stderrText := ""
		if stderr != nil {
			stderrText = stderr.String()
		}
		w.err = mediaexec.Error("ffmpeg", w.args, stderrText, cmdErr)
		return
	}

	if uploadedSegments == 0 {
		w.state = WorkerStateError
		w.err = fmt.Errorf("ffmpeg produced no segments")
		return
	}
	if len(w.expected) > 0 {
		for index := range w.expected {
			if _, ok := uploaded[index]; !ok {
				w.state = WorkerStateError
				w.err = fmt.Errorf("ffmpeg did not produce expected segment %d", index)
				return
			}
		}
		for index := range uploaded {
			if _, ok := w.expected[index]; !ok {
				w.state = WorkerStateError
				w.err = fmt.Errorf("ffmpeg produced unexpected segment %d", index)
				return
			}
		}
	}

	w.state = WorkerStateDone
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

func (w *Worker) setError(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.state = WorkerStateError
	w.err = err
}
