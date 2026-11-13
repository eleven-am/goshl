package goshl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func installVersionedBinaries(t *testing.T, version string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		script := "#!/bin/sh\nif [ \"$1\" = \"-version\" ]; then echo \"" + name + " version " + version + "\"; fi\nexit 0\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0755); err != nil {
			t.Fatalf("write %s stub: %v", name, err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func versionTestOptions() Options {
	return Options{
		Storage:     &stubStorage{},
		Coordinator: &stubCoordinator{},
		PathGen:     stubPathGen{},
	}
}

func TestStartupRejectsOldFFmpeg(t *testing.T) {
	installVersionedBinaries(t, "6.0")
	controller, err := NewControllerWithContext(context.Background(), versionTestOptions())
	if !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("err = %v, want ErrInvalidOptions", err)
	}
	if controller != nil {
		t.Fatal("controller must be nil on error")
	}
}

func TestStartupAcceptsSupportedFFmpeg(t *testing.T) {
	installVersionedBinaries(t, "6.1")
	controller, err := NewControllerWithContext(context.Background(), versionTestOptions())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = controller
}
