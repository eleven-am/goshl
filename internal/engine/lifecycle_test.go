package engine

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func TestStopKillsDetachedAssetGeneration(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	script := "#!/bin/sh\nif [ \"$1\" = \"-version\" ]; then exit 0; fi\necho $$ > " + pidFile + ".tmp\nmv " + pidFile + ".tmp " + pidFile + "\nexec sleep 60\n"
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	controller := lifecycleController(t, newClusterCoordinator(), time.Minute)
	requestDone := make(chan error, 1)
	go func() {
		_, err := controller.Thumbnail(context.Background(), "file:///media", nil)
		requestDone <- err
	}()

	var pid int
	waitUntil(t, 10*time.Second, func() bool {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		parsed, err := strconv.Atoi(strings.TrimSpace(string(data)))
		pid = parsed
		return err == nil
	})
	if !processAlive(pid) {
		t.Fatalf("fake ffmpeg %d is not running", pid)
	}

	started := time.Now()
	controller.Stop()
	if elapsed := time.Since(started); elapsed > 15*time.Second {
		t.Fatalf("Stop took %v", elapsed)
	}
	waitUntil(t, 5*time.Second, func() bool { return !processAlive(pid) })
	select {
	case err := <-requestDone:
		if err == nil {
			t.Fatal("expected the in-flight request to fail once the controller stopped")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request did not return after Stop")
	}
}
