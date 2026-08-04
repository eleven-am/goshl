package mediaexec

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
)

const maxOutputBytes = 64 << 20

type LimitedBuffer struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func NewLimitedBuffer(limit int) *LimitedBuffer {
	return &LimitedBuffer{limit: limit}
}

func (b *LimitedBuffer) Write(data []byte) (int, error) {
	original := len(data)
	remaining := b.limit - b.buffer.Len()
	if remaining <= 0 {
		b.truncated = true
		return original, nil
	}
	if len(data) > remaining {
		data = data[:remaining]
		b.truncated = true
	}
	_, _ = b.buffer.Write(data)
	return original, nil
}

func (b *LimitedBuffer) String() string {
	value := b.buffer.String()
	if b.truncated {
		value += "\n[output truncated]"
	}
	return value
}

func (b *LimitedBuffer) Bytes() []byte {
	return b.buffer.Bytes()
}

func Run(ctx context.Context, name string, args ...string) error {
	stderr := NewLimitedBuffer(64 << 10)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return commandError(name, args, stderr.String(), err)
	}
	return nil
}

func Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	stderr := NewLimitedBuffer(64 << 10)
	stdout := NewLimitedBuffer(maxOutputBytes)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = stderr
	cmd.Stdout = stdout
	if err := cmd.Run(); err != nil {
		return nil, commandError(name, args, stderr.String(), err)
	}
	if stdout.truncated {
		return nil, fmt.Errorf("%s output exceeded %d bytes", name, maxOutputBytes)
	}
	return append([]byte(nil), stdout.Bytes()...), nil
}

func StartOutput(ctx context.Context, name string, args ...string) (*exec.Cmd, io.ReadCloser, *LimitedBuffer, error) {
	stderr := NewLimitedBuffer(64 << 10)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, stderr, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, stderr, commandError(name, args, stderr.String(), err)
	}
	return cmd, stdout, stderr, nil
}

func Error(name string, args []string, stderr string, err error) error {
	return commandError(name, args, stderr, err)
}

func commandError(name string, args []string, stderr string, err error) error {
	detail := redactText(strings.TrimSpace(stderr), args)
	command := strings.Join(redactArgs(args), " ")
	if detail == "" {
		return fmt.Errorf("%s %s: %w", name, command, err)
	}
	return fmt.Errorf("%s %s: %w: %s", name, command, err, detail)
}

func redactText(value string, args []string) string {
	for index, argument := range args {
		if argument == "" {
			continue
		}
		isInput := index > 0 && args[index-1] == "-i"
		isURL := hasMediaScheme(argument)
		if isInput || isURL || filepath.IsAbs(argument) {
			value = strings.ReplaceAll(value, argument, "<redacted-media>")
		}
	}
	return value
}

func redactArgs(args []string) []string {
	redacted := append([]string(nil), args...)
	for index, argument := range redacted {
		if index > 0 && redacted[index-1] == "-i" {
			redacted[index] = "<redacted-input>"
			continue
		}
		if hasMediaScheme(argument) {
			redacted[index] = "<redacted-url>"
			continue
		}
		if strings.HasPrefix(argument, "/") && index == len(redacted)-1 {
			redacted[index] = "<redacted-path>"
		}
	}
	return redacted
}

func hasMediaScheme(value string) bool {
	colon := strings.IndexByte(value, ':')
	if colon <= 0 || value[:colon] == "lavfi" {
		return false
	}
	for index := 0; index < colon; index++ {
		char := value[index]
		if index == 0 && (char < 'A' || char > 'Z') && (char < 'a' || char > 'z') {
			return false
		}
		if (char < 'A' || char > 'Z') && (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '+' && char != '-' && char != '.' {
			return false
		}
	}
	return true
}
