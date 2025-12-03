package transcode

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/eleven-am/goshl/internal/domain"
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
	sourceURL string
	rendition string
	isVideo   bool
	tmpDir    string
	skipFirst bool

	mu     sync.RWMutex
	state  WorkerState
	err    error
	cmd    *exec.Cmd
	cancel context.CancelFunc
}

func NewWorker(args []string, storage domain.Storage, sourceURL string, rendition string, isVideo bool, tmpDir string, skipFirst bool) *Worker {
	return &Worker{
		args:      args,
		storage:   storage,
		sourceURL: sourceURL,
		rendition: rendition,
		isVideo:   isVideo,
		tmpDir:    tmpDir,
		skipFirst: skipFirst,
		state:     WorkerStateIdle,
	}
}

func (w *Worker) Start(ctx context.Context) error {
	w.mu.Lock()
	if w.state != WorkerStateIdle {
		w.mu.Unlock()
		return fmt.Errorf("worker already started")
	}
	w.state = WorkerStateRunning
	w.mu.Unlock()

	ctx, w.cancel = context.WithCancel(ctx)

	w.cmd = exec.CommandContext(ctx, "ffmpeg", w.args...)
	stdout, err := w.cmd.StdoutPipe()
	if err != nil {
		w.setError(err)
		return err
	}

	if err := w.cmd.Start(); err != nil {
		w.setError(err)
		return err
	}

	go w.run(ctx, stdout)

	return nil
}

func (w *Worker) run(ctx context.Context, stdout interface{}) {
	reader, ok := stdout.(interface{ Read([]byte) (int, error) })
	if !ok {
		w.setError(fmt.Errorf("invalid stdout type"))
		return
	}

	scanner := bufio.NewScanner(reader)
	skipFirst := w.skipFirst

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			w.cmd.Wait()
			return
		default:
		}

		filename := strings.TrimSpace(scanner.Text())
		if filename == "" {
			continue
		}

		if skipFirst {
			skipFirst = false
			os.Remove(filepath.Join(w.tmpDir, filename))
			continue
		}

		if err := w.uploadSegment(ctx, filename); err != nil {
			w.setError(err)
			w.cmd.Wait()
			return
		}
	}

	cmdErr := w.cmd.Wait()

	w.mu.Lock()
	defer w.mu.Unlock()

	if cmdErr != nil && ctx.Err() == nil {
		w.state = WorkerStateError
		w.err = cmdErr
		return
	}

	w.state = WorkerStateDone
}

func (w *Worker) uploadSegment(ctx context.Context, filename string) error {
	idx, err := parseSegmentIndex(filename)
	if err != nil {
		return nil
	}

	filePath := filepath.Join(w.tmpDir, filename)

	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("read segment file %s: %w", filename, err)
	}

	info := domain.SegmentData{
		SourceURL: w.sourceURL,
		Index:     idx,
		Rendition: w.rendition,
		IsVideo:   w.isVideo,
	}

	if err := w.storage.WriteSegment(ctx, info, data); err != nil {
		return fmt.Errorf("write segment %d: %w", idx, err)
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
	return strconv.Atoi(parts[len(parts)-1])
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
