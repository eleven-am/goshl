package misc

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/coordination/coordtest"
	"github.com/eleven-am/goshl/internal/testmedia"
)

type meteredResponse struct {
	http.ResponseWriter
	served *atomic.Int64
}

func (w meteredResponse) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.served.Add(int64(n))
	return n, err
}

func serveMetered(t *testing.T, path string) (string, *atomic.Int64) {
	t.Helper()
	served := &atomic.Int64{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		file, err := os.Open(path)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusNotFound)
			return
		}
		defer file.Close()
		http.ServeContent(meteredResponse{writer, served}, request, filepath.Base(path), time.Time{}, file)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/" + filepath.Base(path), served
}

func matroskaSubtitleFixture(t *testing.T, name string, extra ...string) string {
	t.Helper()
	testmedia.Require(t)
	dir := t.TempDir()
	texts := []string{"Plain line %d", "<i>Italic %d</i>", "Two lines %d\nsecond line", "{\\an8}Top %d", "<font color=\"#ff0000\">Red %d</font> & <b>bold</b>", "Arrow --> %d", "Ünïcödé %d 日本語"}
	inputs := []string{"-v", "error", "-y", "-f", "lavfi", "-t", "60", "-i", "testsrc2=size=640x360:rate=25,noise=alls=40:allf=t", "-f", "lavfi", "-t", "60", "-i", "sine=frequency=440"}
	for track := 0; track < 3; track++ {
		var script strings.Builder
		for cue := 0; cue < 80; cue++ {
			start := 700 + 150*track + cue*700
			if cue%9 == 8 {
				start -= 700
			}
			text := fmt.Sprintf(texts[(cue+track)%len(texts)], cue)
			fmt.Fprintf(&script, "%d\n%s --> %s\n%s\n\n", cue+1, subtitleClock(start), subtitleClock(start+300+20*(cue%20)), text)
		}
		path := filepath.Join(dir, fmt.Sprintf("%d.srt", track))
		if err := os.WriteFile(path, []byte(script.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, "-i", path)
	}
	out := filepath.Join(dir, name)
	args := append(inputs, "-map", "0:v", "-map", "1:a", "-map", "2", "-map", "3", "-map", "4", "-c:v", "libx264", "-preset", "ultrafast", "-g", "50", "-b:v", "4M", "-minrate", "4M", "-maxrate", "4M", "-bufsize", "2M", "-x264-params", "nal-hrd=cbr", "-c:a", "aac", "-c:s:0", "srt", "-c:s:1", "ass", "-c:s:2", "srt")
	args = append(append(args, extra...), out)
	if data, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v: %s", err, data)
	}
	return out
}

func subtitleClock(milliseconds int) string {
	return fmt.Sprintf("%02d:%02d:%02d,%03d", milliseconds/3600000, milliseconds/60000%60, milliseconds/1000%60, milliseconds%1000)
}

func matroskaSubtitleJobs(origin float64, codecs ...string) []SubtitleJob {
	jobs := make([]SubtitleJob, 0, len(codecs))
	for i, codec := range codecs {
		jobs = append(jobs, SubtitleJob{Key: "m", TrackID: fmt.Sprintf("s%d", i), StreamIndex: 2 + i, Codec: codec, Origin: origin})
	}
	return jobs
}

func fullPassReference(t *testing.T, path string, jobs []SubtitleJob) map[string][]byte {
	t.Helper()
	store := newStrictVTTStore()
	gen := NewGeneratorWithCoordinator(store, coordtest.NewMemCoordinator(), nil)
	failures := map[string]error{}
	if err := gen.extractSubtitleBatchFFmpeg(context.Background(), path, jobs, failures); err != nil || len(failures) != 0 {
		t.Fatalf("full ffmpeg pass: %v %v", err, failures)
	}
	reference := map[string][]byte{}
	for _, job := range jobs {
		reference[job.TrackID] = store.data["m/"+job.TrackID]
		if bytes.Count(reference[job.TrackID], []byte(" --> ")) < 70 {
			t.Fatalf("reference for %s holds too few cues:\n%s", job.TrackID, reference[job.TrackID])
		}
	}
	return reference
}

func TestMatroskaSubtitlesEqualTheFullPassWithoutReadingTheFile(t *testing.T) {
	fixtures := map[string][]string{
		"start at zero":     nil,
		"start at 7.3 s":    {"-output_ts_offset", "7.3"},
		"one track at once": nil,
	}
	for name, extra := range fixtures {
		t.Run(name, func(t *testing.T) {
			path := matroskaSubtitleFixture(t, "subtitled.mkv", extra...)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			jobs := matroskaSubtitleJobs(testmedia.FormatStart(t, path), "subrip", "ass", "subrip")
			reference := fullPassReference(t, path, jobs)
			if name == "one track at once" {
				jobs = jobs[2:]
			}

			url, served := serveMetered(t, path)
			gen := NewGeneratorWithCoordinator(newStrictVTTStore(), coordtest.NewMemCoordinator(), nil)
			outcomes, err := gen.GetSubtitlesBatch(context.Background(), url, jobs)
			if err != nil {
				t.Fatalf("batch: %v", err)
			}
			for _, job := range jobs {
				outcome := outcomes[job.TrackID]
				if outcome.Err != nil {
					t.Fatalf("track %s: %v", job.TrackID, outcome.Err)
				}
				if !bytes.Equal(outcome.Data, reference[job.TrackID]) {
					t.Fatalf("track %s differs from the full ffmpeg pass:\n%s\nwant:\n%s", job.TrackID, outcome.Data, reference[job.TrackID])
				}
			}
			if got, limit := served.Load(), info.Size()/5; got > limit {
				t.Fatalf("extraction downloaded %d of %d bytes, limit %d", got, info.Size(), limit)
			}
		})
	}
}

func TestMatroskaSubtitlesFallBackToTheFullPass(t *testing.T) {
	cases := map[string]struct {
		extra  []string
		codecs []string
		patch  bool
	}{
		"writer that may not cue every packet": {nil, []string{"subrip", "ass", "subrip"}, true},
		"webvtt track":                         {[]string{"-c:s:1", "webvtt"}, []string{"subrip", "webvtt", "subrip"}, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			path := matroskaSubtitleFixture(t, "subtitled.mkv", c.extra...)
			if c.patch {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, bytes.Replace(data, []byte("Lavf"), []byte("Xavf"), 1), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			jobs := matroskaSubtitleJobs(testmedia.FormatStart(t, path), c.codecs...)
			reference := fullPassReference(t, path, jobs)
			url, served := serveMetered(t, path)
			gen := NewGeneratorWithCoordinator(newStrictVTTStore(), coordtest.NewMemCoordinator(), nil)
			outcomes, err := gen.GetSubtitlesBatch(context.Background(), url, jobs)
			if err != nil {
				t.Fatalf("batch: %v", err)
			}
			for _, job := range jobs {
				if outcome := outcomes[job.TrackID]; outcome.Err != nil || !bytes.Equal(outcome.Data, reference[job.TrackID]) {
					t.Fatalf("track %s: err %v, equal %v", job.TrackID, outcome.Err, bytes.Equal(outcome.Data, reference[job.TrackID]))
				}
			}
			if served.Load() < info.Size() {
				t.Fatalf("expected a full pass over %d bytes, only %d were read", info.Size(), served.Load())
			}
		})
	}
}

func countingFFmpeg(t *testing.T) string {
	t.Helper()
	real, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\ncase \"$1\" in -version) exec \"" + real + "\" \"$@\";; esac\nn=0\nfor a; do [ \"$a\" = \"-map\" ] && n=$((n+1)); done\necho $n >> \"" + log + "\"\nexec \"" + real + "\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func TestSubtitleBatchRunsOneFFmpegPassForAllFallbackTracks(t *testing.T) {
	cases := map[string]struct {
		extra  []string
		codecs []string
		patch  bool
		want   []string
	}{
		"all tracks need the full pass":     {nil, []string{"subrip", "ass", "subrip"}, true, []string{"3"}},
		"one track needs the full pass":     {[]string{"-c:s:1", "webvtt"}, []string{"subrip", "webvtt", "subrip"}, false, []string{"2", "1"}},
		"two tracks need the full pass":     {[]string{"-c:s:0", "webvtt", "-c:s:2", "webvtt"}, []string{"webvtt", "ass", "webvtt"}, false, []string{"1", "2"}},
		"every track is a webvtt full pass": {[]string{"-c:s:0", "webvtt", "-c:s:1", "webvtt", "-c:s:2", "webvtt"}, []string{"webvtt", "webvtt", "webvtt"}, false, []string{"3"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			path := matroskaSubtitleFixture(t, "subtitled.mkv", c.extra...)
			if c.patch {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, bytes.Replace(data, []byte("Lavf"), []byte("Xavf"), 1), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			jobs := matroskaSubtitleJobs(testmedia.FormatStart(t, path), c.codecs...)
			log := countingFFmpeg(t)
			gen := NewGeneratorWithCoordinator(newStrictVTTStore(), coordtest.NewMemCoordinator(), nil)
			outcomes, err := gen.GetSubtitlesBatch(context.Background(), path, jobs)
			if err != nil {
				t.Fatalf("batch: %v", err)
			}
			for _, job := range jobs {
				if outcomes[job.TrackID].Err != nil {
					t.Fatalf("track %s: %v", job.TrackID, outcomes[job.TrackID].Err)
				}
			}
			raw, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Fields(string(raw)); strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Fatalf("ffmpeg invocations carried %v maps, want %v", got, c.want)
			}
		})
	}
}
