package misc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/eleven-am/goshl/internal/mediaexec"
	"github.com/eleven-am/goshl/internal/source"
	"github.com/eleven-am/goshl/internal/subtitle"
	"github.com/eleven-am/goshl/internal/webvtt"
)

type SubtitleJob struct {
	Key         string
	TrackID     string
	StreamIndex int
	Codec       string
	Origin      float64
}

var ErrSubtitleNotConvertible = errors.New("subtitle codec cannot be converted to webvtt")

type SubtitleRequest struct {
	SourceKey string
	Jobs      []SubtitleJob
	Siblings  []SubtitleJob
}

const subtitleFlightAttempts = 3

type SubtitleOutcome struct {
	Data []byte
	Err  error
}

type batchFailure struct {
	tracks map[string]error
}

func (f *batchFailure) Error() string {
	ids := make([]string, 0, len(f.tracks))
	for id := range f.tracks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, id+": "+f.tracks[id].Error())
	}
	return "subtitle extraction failed: " + strings.Join(parts, "; ")
}

func (g *Generator) GetSubtitlesBatch(ctx context.Context, sourceURL string, jobs []SubtitleJob) (map[string]SubtitleOutcome, error) {
	return g.GetSubtitles(ctx, sourceURL, SubtitleRequest{Jobs: jobs})
}

func (g *Generator) GetSubtitles(ctx context.Context, sourceURL string, request SubtitleRequest) (map[string]SubtitleOutcome, error) {
	outcomes := make(map[string]SubtitleOutcome, len(request.Jobs))
	var pending []SubtitleJob
	for _, job := range request.Jobs {
		if _, seen := outcomes[job.TrackID]; seen {
			continue
		}
		exists, err := g.storage.SubtitleVTTExists(ctx, job.Key, job.TrackID)
		if err != nil {
			return nil, fmt.Errorf("check subtitle vtt: %w", err)
		}
		if exists {
			data, err := g.storage.ReadSubtitleVTT(ctx, job.Key, job.TrackID)
			outcomes[job.TrackID] = SubtitleOutcome{Data: data, Err: err}
			continue
		}
		if !subtitle.Convertible(job.Codec) {
			outcomes[job.TrackID] = SubtitleOutcome{Err: fmt.Errorf("%w: %q", ErrSubtitleNotConvertible, job.Codec)}
			continue
		}
		outcomes[job.TrackID] = SubtitleOutcome{}
		pending = append(pending, job)
	}
	if len(pending) == 0 {
		return outcomes, nil
	}

	failures := map[string]error{}
	var err error
	for waiting, attempt := pending, 1; ; attempt++ {
		var missing []SubtitleJob
		missing, err = g.subtitleFlight(ctx, sourceURL, request, waiting, failures)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil || len(missing) == 0 || request.SourceKey == "" || attempt == subtitleFlightAttempts {
			break
		}
		waiting = missing
	}
	for _, job := range pending {
		if failure := failures[job.TrackID]; failure != nil {
			outcomes[job.TrackID] = SubtitleOutcome{Err: failure}
			continue
		}
		if err != nil {
			outcomes[job.TrackID] = SubtitleOutcome{Err: err}
			continue
		}
		data, readErr := g.storage.ReadSubtitleVTT(ctx, job.Key, job.TrackID)
		outcomes[job.TrackID] = SubtitleOutcome{Data: data, Err: readErr}
	}
	return outcomes, nil
}

func (g *Generator) subtitleFlight(ctx context.Context, sourceURL string, request SubtitleRequest, pending []SubtitleJob, failures map[string]error) ([]SubtitleJob, error) {
	key := batchKey(pending)
	if request.SourceKey != "" {
		key = "subtitles:" + request.SourceKey
	}
	err := g.runner().Once(ctx, &g.subtitles, key, func(readyCtx context.Context) (bool, error) {
		for _, job := range pending {
			ok, err := g.storage.SubtitleVTTExists(readyCtx, job.Key, job.TrackID)
			if err != nil || !ok {
				return false, err
			}
		}
		return true, nil
	}, func(genCtx context.Context) error {
		failed := make(map[string]error)
		if err := g.runSubtitleExtraction(genCtx, sourceURL, pending, request.Siblings, failed); err != nil {
			return err
		}
		if len(failed) > 0 {
			return &batchFailure{tracks: failed}
		}
		return nil
	})
	var failed *batchFailure
	if errors.As(err, &failed) {
		for id, failure := range failed.tracks {
			failures[id] = failure
		}
		err = nil
	}
	if err != nil || ctx.Err() != nil || request.SourceKey == "" {
		return nil, err
	}
	var missing []SubtitleJob
	for _, job := range pending {
		if failures[job.TrackID] != nil {
			continue
		}
		exists, err := g.storage.SubtitleVTTExists(ctx, job.Key, job.TrackID)
		if err != nil {
			return nil, fmt.Errorf("check subtitle vtt: %w", err)
		}
		if !exists {
			missing = append(missing, job)
		}
	}
	return missing, nil
}

func batchKey(jobs []SubtitleJob) string {
	keys := make([]string, 0, len(jobs))
	for _, job := range jobs {
		keys = append(keys, job.Key+"\x00"+job.TrackID)
	}
	sort.Strings(keys)
	sum := sha256.Sum256([]byte(strings.Join(keys, "\x01")))
	return "subtitles:" + hex.EncodeToString(sum[:])
}

func (g *Generator) runSubtitleExtraction(ctx context.Context, sourceURL string, jobs, siblings []SubtitleJob, failures map[string]error) error {
	if g.extractSubtitles != nil {
		return g.extractSubtitles(ctx, sourceURL, jobs, failures)
	}
	return g.extractSubtitleBatch(ctx, sourceURL, jobs, siblings, failures)
}

func (g *Generator) extractSubtitleBatch(ctx context.Context, sourceURL string, jobs, siblings []SubtitleJob, failures map[string]error) error {
	tracks := make([]subtitle.Track, 0, len(jobs))
	for _, job := range jobs {
		tracks = append(tracks, subtitle.Track{ID: job.TrackID, StreamIndex: job.StreamIndex, Codec: job.Codec, Origin: time.Duration(math.Round(job.Origin * float64(time.Second)))})
	}
	indexed, err := subtitle.ExtractBatch(ctx, sourceURL, tracks)
	if err == nil {
		remaining := make([]SubtitleJob, 0, len(jobs))
		for _, job := range jobs {
			outcome := indexed[job.TrackID]
			if !outcome.Supported {
				remaining = append(remaining, job)
				continue
			}
			if outcome.Err != nil {
				failures[job.TrackID] = outcome.Err
				continue
			}
			if err := g.storage.WriteSubtitleVTT(ctx, job.Key, job.TrackID, outcome.Data); err != nil {
				failures[job.TrackID] = fmt.Errorf("write subtitle vtt: %w", err)
			}
		}
		jobs = remaining
	} else if !errors.Is(err, subtitle.ErrUnsupported) {
		return fmt.Errorf("indexed subtitle extraction: %w", err)
	}
	if len(jobs) == 0 {
		return nil
	}
	jobs, err = g.extractCompactMatroska(ctx, sourceURL, jobs, failures)
	if err != nil || len(jobs) == 0 {
		return err
	}
	return g.extractFullPass(ctx, sourceURL, jobs, siblings, failures)
}

func (g *Generator) extractFullPass(ctx context.Context, sourceURL string, jobs, siblings []SubtitleJob, failures map[string]error) error {
	extra := g.uncachedSiblings(ctx, sourceURL, jobs, siblings)
	if len(extra) == 0 {
		return g.extractSubtitleBatchFFmpeg(ctx, sourceURL, jobs, failures)
	}
	dir, err := os.MkdirTemp("", "goshl-subtitles-")
	if err != nil {
		return fmt.Errorf("create subtitle workdir: %w", err)
	}
	defer os.RemoveAll(dir)

	input, err := source.InputArgs(sourceURL)
	if err != nil {
		return err
	}
	all := append(append([]SubtitleJob{}, jobs...), extra...)
	paths := make([]string, len(all))
	for i := range all {
		paths[i] = filepath.Join(dir, fmt.Sprintf("wide-%d.vtt", i))
	}
	if err := mediaexec.Run(ctx, "ffmpeg", subtitleArgs(input, all, paths)...); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		g.narrowSubtitles.Store(sourceURL, struct{}{})
		return g.extractSubtitleBatchFFmpeg(ctx, sourceURL, jobs, failures)
	}
	g.storeConverted(ctx, jobs, paths[:len(jobs)], failures, false)
	g.storeConverted(ctx, extra, paths[len(jobs):], make(map[string]error), false)
	return nil
}

func (g *Generator) uncachedSiblings(ctx context.Context, sourceURL string, jobs, siblings []SubtitleJob) []SubtitleJob {
	if _, narrowed := g.narrowSubtitles.Load(sourceURL); narrowed {
		return nil
	}
	taken := make(map[string]bool, len(jobs)+len(siblings))
	for _, job := range jobs {
		taken[job.TrackID] = true
	}
	var extra []SubtitleJob
	for _, sibling := range siblings {
		if taken[sibling.TrackID] || !subtitle.Convertible(sibling.Codec) {
			continue
		}
		taken[sibling.TrackID] = true
		if exists, err := g.storage.SubtitleVTTExists(ctx, sibling.Key, sibling.TrackID); err != nil || exists {
			continue
		}
		extra = append(extra, sibling)
	}
	return extra
}

func (g *Generator) extractCompactMatroska(ctx context.Context, sourceURL string, jobs []SubtitleJob, failures map[string]error) ([]SubtitleJob, error) {
	dir, err := os.MkdirTemp("", "goshl-subtitles-")
	if err != nil {
		return nil, fmt.Errorf("create subtitle workdir: %w", err)
	}
	defer os.RemoveAll(dir)

	tracks := make([]subtitle.Track, 0, len(jobs))
	for _, job := range jobs {
		tracks = append(tracks, subtitle.Track{ID: job.TrackID, StreamIndex: job.StreamIndex, Codec: job.Codec})
	}
	compact, err := subtitle.CompactMatroska(ctx, sourceURL, tracks, dir)
	if err != nil {
		if errors.Is(err, subtitle.ErrUnsupported) {
			return jobs, nil
		}
		return nil, fmt.Errorf("indexed subtitle extraction: %w", err)
	}
	var compacted, remaining []SubtitleJob
	for _, job := range jobs {
		stream, found := compact.Streams[job.TrackID]
		if !found {
			remaining = append(remaining, job)
			continue
		}
		job.StreamIndex = stream
		compacted = append(compacted, job)
	}
	input, err := source.InputArgs(compact.Path)
	if err != nil {
		return nil, err
	}
	attempts := make(map[string]error)
	if err := g.convertSubtitles(ctx, dir, append([]string{"-copyts"}, input...), compacted, attempts, true); err != nil {
		return nil, err
	}
	for _, job := range jobs {
		if attempts[job.TrackID] != nil {
			remaining = append(remaining, job)
		}
	}
	return remaining, nil
}

func (g *Generator) extractSubtitleBatchFFmpeg(ctx context.Context, sourceURL string, jobs []SubtitleJob, failures map[string]error) error {
	dir, err := os.MkdirTemp("", "goshl-subtitles-")
	if err != nil {
		return fmt.Errorf("create subtitle workdir: %w", err)
	}
	defer os.RemoveAll(dir)

	input, err := source.InputArgs(sourceURL)
	if err != nil {
		return err
	}
	return g.convertSubtitles(ctx, dir, input, jobs, failures, false)
}

func (g *Generator) convertSubtitles(ctx context.Context, dir string, input []string, jobs []SubtitleJob, failures map[string]error, sourceTimeline bool) error {
	paths := make([]string, len(jobs))
	for i := range jobs {
		paths[i] = filepath.Join(dir, fmt.Sprintf("batch-%d.vtt", i))
	}
	if err := mediaexec.Run(ctx, "ffmpeg", subtitleArgs(input, jobs, paths)...); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if len(jobs) == 1 {
			failures[jobs[0].TrackID] = fmt.Errorf("ffmpeg subtitle extraction: %w", err)
			return nil
		}
		for i, job := range jobs {
			paths[i] = filepath.Join(dir, fmt.Sprintf("track-%d.vtt", i))
			if runErr := mediaexec.Run(ctx, "ffmpeg", subtitleArgs(input, jobs[i:i+1], paths[i:i+1])...); runErr != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				failures[job.TrackID] = fmt.Errorf("ffmpeg subtitle extraction: %w", runErr)
			}
		}
	}

	g.storeConverted(ctx, jobs, paths, failures, sourceTimeline)
	return nil
}

func (g *Generator) storeConverted(ctx context.Context, jobs []SubtitleJob, paths []string, failures map[string]error, sourceTimeline bool) {
	for i, job := range jobs {
		if failures[job.TrackID] != nil {
			continue
		}
		raw, err := os.ReadFile(paths[i])
		if err != nil {
			failures[job.TrackID] = fmt.Errorf("read extracted subtitle: %w", err)
			continue
		}
		cues, err := webvtt.Parse(raw)
		if err != nil {
			failures[job.TrackID] = fmt.Errorf("parse extracted subtitle: %w", err)
			continue
		}
		if sourceTimeline {
			cues = webvtt.Shift(cues, time.Duration(math.Round(job.Origin*float64(time.Second))))
		}
		data := webvtt.Encode(cues, webvtt.Options{TimestampMap: true})
		if err := g.storage.WriteSubtitleVTT(ctx, job.Key, job.TrackID, data); err != nil {
			failures[job.TrackID] = fmt.Errorf("write subtitle vtt: %w", err)
		}
	}
}

func subtitleArgs(input []string, jobs []SubtitleJob, paths []string) []string {
	args := append([]string{"-nostdin", "-y"}, input...)
	for i, job := range jobs {
		args = append(args, "-map", fmt.Sprintf("0:%d", job.StreamIndex), "-c:s", "webvtt", "-f", "webvtt", paths[i])
	}
	return args
}
