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

type SubtitleOutcome struct {
	Data []byte
	Err  error
}

var bitmapSubtitleCodecs = map[string]struct{}{
	"hdmv_pgs_subtitle": {},
	"pgssub":            {},
	"dvd_subtitle":      {},
	"dvdsub":            {},
	"dvb_subtitle":      {},
	"dvbsub":            {},
	"xsub":              {},
}

func (g *Generator) GetSubtitlesBatch(ctx context.Context, sourceURL string, jobs []SubtitleJob) (map[string]SubtitleOutcome, error) {
	outcomes := make(map[string]SubtitleOutcome, len(jobs))
	var pending []SubtitleJob
	for _, job := range jobs {
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
		if _, bitmap := bitmapSubtitleCodecs[job.Codec]; bitmap {
			outcomes[job.TrackID] = SubtitleOutcome{Err: fmt.Errorf("subtitle codec %s cannot be converted to webvtt", job.Codec)}
			continue
		}
		outcomes[job.TrackID] = SubtitleOutcome{}
		pending = append(pending, job)
	}
	if len(pending) == 0 {
		return outcomes, nil
	}

	failures := make(map[string]error)
	err := g.runner().Once(ctx, &g.subtitles, batchKey(pending), func(readyCtx context.Context) (bool, error) {
		for _, job := range pending {
			ok, err := g.storage.SubtitleVTTExists(readyCtx, job.Key, job.TrackID)
			if err != nil || !ok {
				return false, err
			}
		}
		return true, nil
	}, func(genCtx context.Context) error {
		return g.runSubtitleExtraction(genCtx, sourceURL, pending, failures)
	})
	if ctx.Err() != nil {
		return nil, ctx.Err()
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

func batchKey(jobs []SubtitleJob) string {
	keys := make([]string, 0, len(jobs))
	for _, job := range jobs {
		keys = append(keys, job.Key+"\x00"+job.TrackID)
	}
	sort.Strings(keys)
	sum := sha256.Sum256([]byte(strings.Join(keys, "\x01")))
	return "subtitles:" + hex.EncodeToString(sum[:])
}

func (g *Generator) runSubtitleExtraction(ctx context.Context, sourceURL string, jobs []SubtitleJob, failures map[string]error) error {
	if g.extractSubtitles != nil {
		return g.extractSubtitles(ctx, sourceURL, jobs, failures)
	}
	return g.extractSubtitleBatch(ctx, sourceURL, jobs, failures)
}

func (g *Generator) extractSubtitleBatch(ctx context.Context, sourceURL string, jobs []SubtitleJob, failures map[string]error) error {
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
	return g.extractSubtitleBatchFFmpeg(ctx, sourceURL, jobs, failures)
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
	args := append([]string{}, input...)
	paths := make([]string, len(jobs))
	for i, job := range jobs {
		paths[i] = filepath.Join(dir, fmt.Sprintf("%d.vtt", i))
		args = append(args, "-map", fmt.Sprintf("0:%d", job.StreamIndex), "-c:s", "webvtt", "-f", "webvtt", paths[i])
	}

	if err := mediaexec.Run(ctx, "ffmpeg", args...); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		for i, job := range jobs {
			single := append(append([]string{}, input...), "-map", fmt.Sprintf("0:%d", job.StreamIndex), "-c:s", "webvtt", "-f", "webvtt", paths[i])
			if runErr := mediaexec.Run(ctx, "ffmpeg", single...); runErr != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				failures[job.TrackID] = fmt.Errorf("ffmpeg subtitle extraction: %w", runErr)
			}
		}
	}

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
		data := webvtt.Encode(cues, webvtt.Options{TimestampMap: true})
		if err := g.storage.WriteSubtitleVTT(ctx, job.Key, job.TrackID, data); err != nil {
			failures[job.TrackID] = fmt.Errorf("write subtitle vtt: %w", err)
		}
	}
	return nil
}
