package live

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionDirErrorCarriesNoPath(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "secret-parent-SENTINEL")
	if err := os.WriteFile(parent, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := sessionDir(parent)
	if err == nil {
		t.Fatal("expected an error when the parent is not a directory")
	}
	if strings.Contains(err.Error(), "SENTINEL") || strings.Contains(err.Error(), "goshl-live-") || strings.Contains(err.Error(), "/") {
		t.Fatalf("session directory error leaks a path: %q", err.Error())
	}
}

func TestSessionDirIsAbsoluteForRelativeParent(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)
	if err := os.Mkdir("rel", 0o700); err != nil {
		t.Fatal(err)
	}
	dir, err := sessionDir("rel")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(dir) {
		t.Fatalf("session directory %q is relative, so ffmpeg error redaction would miss it", dir)
	}
}
