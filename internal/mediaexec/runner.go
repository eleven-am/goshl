package mediaexec

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

func Run(ctx context.Context, name string, args ...string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return commandError(name, args, stderr.String(), err)
	}
	return nil
}

func Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return nil, commandError(name, args, stderr.String(), err)
	}
	return output, nil
}

func StartOutput(ctx context.Context, name string, args ...string) (*exec.Cmd, io.ReadCloser, *bytes.Buffer, error) {
	stderr := &bytes.Buffer{}
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
	detail := strings.TrimSpace(stderr)
	if detail == "" {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, detail)
}
