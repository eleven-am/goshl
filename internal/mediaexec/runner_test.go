package mediaexec

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRedactArgsRemovesInputCredentialsAndPaths(t *testing.T) {
	args := redactArgs([]string{"-ss", "1", "-i", "https://user:secret@example.com/video.mkv?token=secret", "-f", "null", "/private/output"})
	joined := strings.Join(args, " ")
	for _, secret := range []string{"user", "secret", "video.mkv", "/private/output"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("redacted command leaked %q: %s", secret, joined)
		}
	}
}

func TestLimitedBufferCapsRetainedOutput(t *testing.T) {
	buffer := NewLimitedBuffer(4)
	_, _ = buffer.Write([]byte("abcdefgh"))
	if string(buffer.Bytes()) != "abcd" || !strings.Contains(buffer.String(), "truncated") {
		t.Fatalf("unexpected limited buffer: %q", buffer.String())
	}
}

func TestCommandErrorRedactsSecretsFromStderr(t *testing.T) {
	secret := "https://user:password@example.com/video.mkv?token=private"
	err := commandError("ffmpeg", []string{"-i", secret, "-f", "null", "-"}, "unable to read "+secret, assertError("failed"))
	for _, value := range []string{"user", "password", "token", "private", "video.mkv"} {
		if strings.Contains(err.Error(), value) {
			t.Fatalf("command error leaked %q: %v", value, err)
		}
	}
}

func TestCommandErrorRedactsMidCommandPaths(t *testing.T) {
	args := []string{"-i", "file:/media/private/movie.mkv", "-map", "0:v:0", "-c:v", "libx264", "-vf", "scale=1280:720", "-y", "/srv/out/seg_0001.ts", "-f", "segment", "/srv/out/list.m3u8", "http://host/x.m3u8", "-"}
	err := commandError("ffmpeg", args, "cannot open /srv/out/seg_0001.ts and /srv/out/list.m3u8", assertError("failed"))
	text := err.Error()
	for _, leaked := range []string{"/media/private", "movie.mkv", "/srv/out", "seg_0001", "list.m3u8", "host/x"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("command error leaked %q: %s", leaked, text)
		}
	}
	for _, kept := range []string{"0:v:0", "-c:v", "libx264", "scale=1280:720", "segment"} {
		if !strings.Contains(text, kept) {
			t.Fatalf("command error over-redacted %q: %s", kept, text)
		}
	}
}

func TestCommandErrorRedactsHostsPathsAndSegmentNamesDerivedInStderr(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		stderr string
		leaks  []string
		keeps  []string
	}{
		{"remote host and port", []string{"-i", "http://media.internal/a.mkv"}, "Connection to tcp://127.0.0.1:1 failed: Connection refused", []string{"127.0.0.1", "tcp://"}, []string{"Connection refused"}},
		{"unlisted absolute path", []string{"-f", "null", "-"}, "Error opening /var/data/private/x.mp4: No such file or directory", []string{"/var/data", "x.mp4"}, []string{"No such file or directory"}},
		{"quoted absolute path", []string{"-f", "null", "-"}, "Impossible to open '/var/data/private/x.mp4'", []string{"/var/data", "x.mp4"}, []string{"Impossible to open"}},
		{"expanded absolute segment name", []string{"-f", "segment", "/srv/out/seg_%04d.ts"}, "Failed writing /srv/out/seg_0007.ts", []string{"/srv/out", "seg_0007"}, []string{"Failed writing"}},
		{"expanded relative segment name", []string{"-f", "segment", "out/seg_%03d.ts"}, "Failed writing out/seg_012.ts", []string{"seg_012"}, []string{"Failed writing"}},
		{"file scheme in stderr", []string{"-f", "null", "-"}, "Unable to open file:private/clip.mp4 here", []string{"private", "clip.mp4"}, []string{"Unable to open", "here"}},
		{"ordinary text untouched", []string{"-f", "null", "-"}, "Invalid argument w/h and/or bytes=0-0 at 12:30", nil, []string{"Invalid argument w/h and/or bytes=0-0 at 12:30"}},
	}
	for _, tc := range cases {
		text := commandError("ffmpeg", tc.args, tc.stderr, assertError("failed")).Error()
		for _, leaked := range tc.leaks {
			if strings.Contains(text, leaked) {
				t.Fatalf("%s: leaked %q: %s", tc.name, leaked, text)
			}
		}
		for _, kept := range tc.keeps {
			if !strings.Contains(text, kept) {
				t.Fatalf("%s: over-redacted %q: %s", tc.name, kept, text)
			}
		}
	}
}

func TestRedactArgsRedactsFileSchemeArguments(t *testing.T) {
	args := redactArgs([]string{"-attach", "file:relative/cover.jpg", "-metadata", "title=x"})
	if args[1] != "<redacted-url>" {
		t.Fatalf("file: argument must be redacted, got %v", args)
	}
	if args[3] != "title=x" {
		t.Fatalf("ordinary argument must be kept, got %v", args)
	}
	if !hasMediaScheme("file:relative/cover.jpg") || hasMediaScheme("filename") {
		t.Fatal("file: must be recognised as a media scheme only with its colon")
	}
}

func TestRunReturnsAfterCancelWhenChildHoldsPipes(t *testing.T) {
	script := filepath.Join(t.TempDir(), "hold.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30 &\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := Output(ctx, script)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error after cancellation")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Output did not return after cancel while a child held the pipes")
	}
}

type assertError string

func (e assertError) Error() string { return string(e) }
