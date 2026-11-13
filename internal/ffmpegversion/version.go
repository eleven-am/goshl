package ffmpegversion

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

var (
	releasePattern = regexp.MustCompile(`(?m)^\S+ version [nN]?(\d+)\.(\d+)`)
	gitPattern     = regexp.MustCompile(`(?m)^\S+ version (?:[nN]-\d+|git-)`)

	ErrUnrecognized = errors.New("unrecognized version output")
	ErrTooOld       = errors.New("version too old")
)

type Version struct {
	Major int
	Minor int
	Git   bool
}

func Minimum() Version {
	return Version{Major: 6, Minor: 1}
}

func (v Version) AtLeast(min Version) bool {
	if v.Git {
		return true
	}
	if v.Major != min.Major {
		return v.Major > min.Major
	}
	return v.Minor >= min.Minor
}

func (v Version) String() string {
	if v.Git {
		return "git"
	}
	return fmt.Sprintf("%d.%d", v.Major, v.Minor)
}

func Parse(output string) (Version, error) {
	if match := releasePattern.FindStringSubmatch(output); match != nil {
		major, err := strconv.Atoi(match[1])
		if err != nil {
			return Version{}, fmt.Errorf("%w: %v", ErrUnrecognized, err)
		}
		minor, err := strconv.Atoi(match[2])
		if err != nil {
			return Version{}, fmt.Errorf("%w: %v", ErrUnrecognized, err)
		}
		return Version{Major: major, Minor: minor}, nil
	}
	if gitPattern.MatchString(output) {
		return Version{Git: true}, nil
	}
	return Version{}, ErrUnrecognized
}

func Require(ctx context.Context, binary string, min Version) error {
	path, err := exec.LookPath(binary)
	if err != nil {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(probeCtx, path, "-version").Output()
	if err != nil {
		return nil
	}
	version, err := Parse(string(output))
	if err != nil {
		return nil
	}
	if !version.AtLeast(min) {
		return fmt.Errorf("%s %s: %w: need %s or newer", binary, version, ErrTooOld, min)
	}
	return nil
}
