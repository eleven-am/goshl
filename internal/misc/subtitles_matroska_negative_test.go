package misc

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/eleven-am/goshl/internal/coordination/coordtest"
	"github.com/eleven-am/goshl/internal/testmedia"
)

func fullPassOutput(t *testing.T, path string, jobs []SubtitleJob) map[string][]byte {
	t.Helper()
	store := newStrictVTTStore()
	failures := map[string]error{}
	if err := NewGeneratorWithCoordinator(store, coordtest.NewMemCoordinator(), nil).extractSubtitleBatchFFmpeg(context.Background(), path, jobs, failures); err != nil || len(failures) != 0 {
		t.Fatalf("full ffmpeg pass: %v %v", err, failures)
	}
	reference := map[string][]byte{}
	for _, job := range jobs {
		reference[job.TrackID] = store.data["m/"+job.TrackID]
	}
	return reference
}

func TestMatroskaSubtitlesKeepCuesThatStartBeforeTimeZero(t *testing.T) {
	base := matroskaSubtitleFixture(t, "base.mkv")
	path := filepath.Join(t.TempDir(), "cut.mkv")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-y", "-ss", "10.4", "-i", base, "-map", "0", "-c", "copy", "-avoid_negative_ts", "disabled", path).CombinedOutput(); err != nil {
		t.Fatalf("cut fixture: %v: %s", err, out)
	}
	jobs := matroskaSubtitleJobs(testmedia.FormatStart(t, path), "subrip", "ass", "subrip")
	reference := fullPassOutput(t, path, jobs)
	outcomes, err := NewGeneratorWithCoordinator(newStrictVTTStore(), coordtest.NewMemCoordinator(), nil).GetSubtitlesBatch(context.Background(), path, jobs)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	for _, job := range jobs {
		got, want := outcomes[job.TrackID].Data, reference[job.TrackID]
		if outcomes[job.TrackID].Err != nil || !bytes.Equal(got, want) {
			t.Errorf("track %s: err %v, %d cues, the full ffmpeg pass has %d", job.TrackID, outcomes[job.TrackID].Err, bytes.Count(got, []byte(" --> ")), bytes.Count(want, []byte(" --> ")))
		}
	}
}
