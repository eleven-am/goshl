package mediaexec

import (
	"strings"
	"testing"
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

type assertError string

func (e assertError) Error() string { return string(e) }
