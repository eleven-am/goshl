package mediaexec

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const waitDelay = 5 * time.Second

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
	cmd.WaitDelay = waitDelay
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
	cmd.WaitDelay = waitDelay
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
	cmd.WaitDelay = waitDelay
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

const redactedMedia = "<redacted-media>"

var (
	urlPattern     = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.\-]*://[^\s'"<>]+`)
	filePattern    = regexp.MustCompile(`\bfile:[^\s'"<>]*[^\s'"<>:;,.)\]]`)
	pathPattern    = regexp.MustCompile(`(^|[\s'"=(\[,])/[^\s'"<>]*[^\s'"<>:;,.)\]]`)
	sequenceVerbRe = regexp.MustCompile(`%0?\d*d`)
)

func redactText(value string, args []string) string {
	for index, argument := range args {
		if argument == "" {
			continue
		}
		isInput := index > 0 && args[index-1] == "-i"
		isURL := hasMediaScheme(argument)
		if isInput || isURL || filepath.IsAbs(argument) {
			value = strings.ReplaceAll(value, argument, redactedMedia)
		}
		if pattern := sequencePattern(argument); pattern != nil {
			value = pattern.ReplaceAllString(value, redactedMedia)
		}
	}
	value = urlPattern.ReplaceAllString(value, redactedMedia)
	value = filePattern.ReplaceAllString(value, redactedMedia)
	return pathPattern.ReplaceAllString(value, "${1}"+redactedMedia)
}

func sequencePattern(argument string) *regexp.Regexp {
	if !sequenceVerbRe.MatchString(argument) || !strings.ContainsAny(argument, "/.") {
		return nil
	}
	parts := sequenceVerbRe.Split(argument, -1)
	for index, part := range parts {
		parts[index] = regexp.QuoteMeta(part)
	}
	pattern, err := regexp.Compile(strings.Join(parts, `\d+`))
	if err != nil {
		return nil
	}
	return pattern
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
		if filepath.IsAbs(argument) {
			redacted[index] = "<redacted-path>"
		}
	}
	return redacted
}

func hasMediaScheme(value string) bool {
	if strings.HasPrefix(value, "file:") {
		return true
	}
	colon := strings.Index(value, "://")
	if colon <= 0 {
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
