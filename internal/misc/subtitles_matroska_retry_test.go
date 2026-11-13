package misc

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/eleven-am/goshl/internal/coordination/coordtest"
	"github.com/eleven-am/goshl/internal/testmedia"
)

func TestMatroskaSubtitlesUseTheFullPassWhenTheCompactConversionFails(t *testing.T) {
	path := matroskaSubtitleFixture(t, "subtitled.mkv")
	jobs := matroskaSubtitleJobs(testmedia.FormatStart(t, path), "subrip", "ass", "subrip")
	reference := fullPassReference(t, path, jobs)

	real, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	refused := filepath.Join(dir, "refused.log")
	script := "#!/bin/sh\nfor a; do case \"$a\" in */subtitles.mkv) echo refused >> \"" + refused + "\"; exit 1;; esac; done\nexec \"" + real + "\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	outcomes, err := NewGeneratorWithCoordinator(newStrictVTTStore(), coordtest.NewMemCoordinator(), nil).GetSubtitlesBatch(context.Background(), path, jobs)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if log, err := os.ReadFile(refused); err != nil || len(log) == 0 {
		t.Fatalf("the compact conversion was never attempted: %v", err)
	}
	for _, job := range jobs {
		if outcome := outcomes[job.TrackID]; outcome.Err != nil || !bytes.Equal(outcome.Data, reference[job.TrackID]) {
			t.Errorf("track %s: err %v, equal to the full pass %v", job.TrackID, outcome.Err, bytes.Equal(outcome.Data, reference[job.TrackID]))
		}
	}
}
