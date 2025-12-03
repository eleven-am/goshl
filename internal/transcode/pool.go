package transcode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/ffmpeg"
	"github.com/eleven-am/goshl/internal/rendition"
)

type Pool struct {
	coordinator domain.Coordinator
	size        int
	streamType  domain.StreamType
	storage     domain.Storage
	cmdBuilder  *ffmpeg.CommandBuilder
	segStorage  domain.Storage

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewPool(
	coordinator domain.Coordinator,
	size int,
	streamType domain.StreamType,
	storage domain.Storage,
	cmdBuilder *ffmpeg.CommandBuilder,
	segStorage domain.Storage,
) *Pool {
	return &Pool{
		coordinator: coordinator,
		size:        size,
		streamType:  streamType,
		storage:     storage,
		cmdBuilder:  cmdBuilder,
		segStorage:  segStorage,
	}
}

func (p *Pool) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.cancel != nil {
		p.mu.Unlock()
		return fmt.Errorf("pool already started")
	}
	ctx, p.cancel = context.WithCancel(ctx)
	p.mu.Unlock()

	jobs, err := p.coordinator.Subscribe(ctx, p.streamType)
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	for i := 0; i < p.size; i++ {
		p.wg.Add(1)
		go p.worker(ctx, jobs)
	}

	return nil
}

func (p *Pool) Stop() {
	p.mu.Lock()
	if p.cancel != nil {
		p.cancel()
	}
	p.mu.Unlock()

	p.wg.Wait()
}

func (p *Pool) worker(ctx context.Context, jobs <-chan domain.Job) {
	defer p.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case job, ok := <-jobs:
			if !ok {
				return
			}
			p.processJob(ctx, job)
		}
	}
}

func (p *Pool) processJob(ctx context.Context, job domain.Job) {
	meta, err := p.getMetadata(ctx, job.SourceURL)
	if err != nil {
		p.publishError(ctx, job, err)
		return
	}

	segments := p.extractSegments(meta.Keyframes, meta.Duration, job.StartIndex, job.EndIndex)
	if len(segments) == 0 {
		p.publishError(ctx, job, fmt.Errorf("no segments for range %d-%d", job.StartIndex, job.EndIndex))
		return
	}

	tmpDir, err := os.MkdirTemp("", "transcode-*")
	if err != nil {
		p.publishError(ctx, job, fmt.Errorf("create temp dir: %w", err))
		return
	}
	defer os.RemoveAll(tmpDir)

	isVideo := p.streamType == domain.StreamVideo

	var args []string
	var skipFirst bool
	if isVideo {
		videoRendition := p.findVideoRendition(meta, job.Rendition)
		if videoRendition == nil {
			p.publishError(ctx, job, fmt.Errorf("video rendition %s not found", job.Rendition))
			return
		}

		videoSegments := segments
		if job.StartIndex > 0 {
			overlapSegments := p.extractSegments(meta.Keyframes, meta.Duration, job.StartIndex-1, job.EndIndex)
			if len(overlapSegments) > len(segments) {
				videoSegments = overlapSegments
				skipFirst = true
			}
		}

		var actualSeekKeyframe float64
		if videoRendition.Method == domain.DirectStream && len(videoSegments) > 0 {
			actualSeekKeyframe = findNearestKeyframe(meta.Keyframes, videoSegments[0].Start)
		}

		args = p.cmdBuilder.Video(ffmpeg.VideoParams{
			InputURL:           job.SourceURL,
			StreamIndex:        0,
			Rendition:          *videoRendition,
			Segments:           videoSegments,
			OutputDir:          tmpDir,
			ActualSeekKeyframe: actualSeekKeyframe,
		})
	} else {
		audioRendition := p.findAudioRendition(meta, job.Rendition)
		if audioRendition == nil {
			p.publishError(ctx, job, fmt.Errorf("audio rendition %s not found", job.Rendition))
			return
		}
		args = p.cmdBuilder.Audio(ffmpeg.AudioParams{
			InputURL:    job.SourceURL,
			StreamIndex: 0,
			Rendition:   *audioRendition,
			Segments:    segments,
			OutputDir:   tmpDir,
		})
	}

	w := NewWorker(args, p.segStorage, job.SourceURL, job.Rendition, isVideo, tmpDir, skipFirst)
	if err := w.Start(ctx); err != nil {
		p.publishError(ctx, job, err)
		return
	}

	p.waitForWorker(ctx, w)

	p.coordinator.Ack(ctx, job.ID)
}

func (p *Pool) getMetadata(ctx context.Context, sourceURL string) (*domain.Metadata, error) {
	data, err := p.storage.GetMetadata(ctx, sourceURL)
	if err != nil {
		return nil, err
	}
	var meta domain.Metadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

func (p *Pool) waitForWorker(ctx context.Context, w *Worker) {
	ticker := make(chan struct{})
	go func() {
		for w.State() == WorkerStateRunning {
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
		close(ticker)
	}()

	select {
	case <-ctx.Done():
		w.Kill()
	case <-ticker:
	}
}

func (p *Pool) extractSegments(keyframes []float64, duration float64, startIdx, endIdx int) []domain.Segment {
	targetDuration := 6.0
	var segments []domain.Segment
	var idx int

	for i := 0; i < len(keyframes); i++ {
		start := keyframes[i]
		var end float64

		for j := i + 1; j < len(keyframes); j++ {
			if keyframes[j]-start >= targetDuration {
				end = keyframes[j]
				i = j - 1
				break
			}
		}

		if end == 0 {
			if i < len(keyframes)-1 {
				end = keyframes[len(keyframes)-1]
				i = len(keyframes)
			} else {
				end = duration
			}
		}

		if idx >= startIdx && idx <= endIdx {
			segments = append(segments, domain.Segment{
				Index:    idx,
				Start:    start,
				End:      end,
				Duration: end - start,
			})
		}

		idx++
		if idx > endIdx {
			break
		}
	}

	return segments
}

func (p *Pool) findVideoRendition(meta *domain.Metadata, name string) *domain.VideoRendition {
	renditions := rendition.GenerateVideo(meta.Video)
	for _, r := range renditions {
		if r.Name == name {
			return &r
		}
	}
	return nil
}

func (p *Pool) findAudioRendition(meta *domain.Metadata, name string) *domain.AudioRendition {
	if len(meta.Audios) == 0 {
		return nil
	}

	renditions := rendition.GenerateAudio(meta.Audios[0])
	for _, r := range renditions {
		if r.Name == name {
			return &r
		}
	}
	return nil
}

func (p *Pool) publishError(ctx context.Context, job domain.Job, err error) {
	for i := job.StartIndex; i <= job.EndIndex; i++ {
		info := domain.SegmentData{
			SourceURL: job.SourceURL,
			Index:     i,
			Rendition: job.Rendition,
			IsVideo:   p.streamType == domain.StreamVideo,
		}
		status := domain.SegmentStatus{
			State: domain.SegmentStateError,
			Error: err.Error(),
		}

		p.coordinator.NotifySegment(ctx, info, status)
	}
}

func findNearestKeyframe(keyframes []float64, target float64) float64 {
	if len(keyframes) == 0 {
		return 0
	}

	var prevKeyframe float64
	var result float64
	for _, kf := range keyframes {
		if kf <= target+0.001 {
			prevKeyframe = result
			result = kf
		} else {
			break
		}
	}

	if prevKeyframe > 0 && target-result < 0.01 {
		return prevKeyframe
	}
	return result
}
