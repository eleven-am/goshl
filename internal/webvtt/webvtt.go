package webvtt

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
)

const (
	signature       = "WEBVTT"
	cueArrow        = "-->"
	neutralArrow    = "--&gt;"
	mpegTSClockRate = 90000
)

var ErrMalformed = errors.New("malformed webvtt")

type Cue struct {
	ID       string
	Start    time.Duration
	End      time.Duration
	Settings string
	Text     string
}

type Options struct {
	TimestampMap bool
}

func Timestamp(seconds float64) string {
	return FormatDuration(time.Duration(math.Round(seconds*1000)) * time.Millisecond)
}

func FormatDuration(value time.Duration) string {
	if value < 0 {
		value = 0
	}
	totalMilliseconds := (value + time.Millisecond/2) / time.Millisecond
	hours := totalMilliseconds / 3_600_000
	minutes := totalMilliseconds / 60_000 % 60
	seconds := totalMilliseconds / 1_000 % 60
	milliseconds := totalMilliseconds % 1_000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", hours, minutes, seconds, milliseconds)
}

func EscapeText(text string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
}

func TimestampMapLine() string {
	ticks := int64(domain.OutputTimelineOffset) * mpegTSClockRate / int64(time.Second)
	return "X-TIMESTAMP-MAP=MPEGTS:" + strconv.FormatInt(ticks, 10) + ",LOCAL:00:00:00.000"
}

func Shift(cues []Cue, origin time.Duration) []Cue {
	shifted := make([]Cue, 0, len(cues))
	for _, cue := range cues {
		cue.Start = max(cue.Start-origin, 0)
		cue.End = max(cue.End-origin, 0)
		if cue.End <= cue.Start {
			continue
		}
		shifted = append(shifted, cue)
	}
	return shifted
}

func Encode(cues []Cue, options Options) []byte {
	var out bytes.Buffer
	out.WriteString(signature)
	out.WriteByte('\n')
	if options.TimestampMap {
		out.WriteString(TimestampMapLine())
		out.WriteByte('\n')
	}
	out.WriteByte('\n')
	for _, cue := range cues {
		text := cleanPayload(cue.Text)
		if text == "" {
			continue
		}
		if id := singleLine(cue.ID); id != "" {
			out.WriteString(id)
			out.WriteByte('\n')
		}
		out.WriteString(FormatDuration(cue.Start))
		out.WriteString(" " + cueArrow + " ")
		out.WriteString(FormatDuration(cue.End))
		if settings := singleLine(cue.Settings); settings != "" {
			out.WriteByte(' ')
			out.WriteString(settings)
		}
		out.WriteByte('\n')
		out.WriteString(text)
		out.WriteString("\n\n")
	}
	return out.Bytes()
}

func Parse(data []byte) ([]Cue, error) {
	text := strings.TrimPrefix(string(data), "\xef\xbb\xbf")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	blocks := splitBlocks(text)
	if len(blocks) == 0 || !isSignature(blocks[0][0]) {
		return nil, fmt.Errorf("%w: missing %s signature", ErrMalformed, signature)
	}
	var cues []Cue
	for _, block := range blocks[1:] {
		first := block[0]
		if isNonCueBlock(first) {
			continue
		}
		timingAt := 0
		if !strings.Contains(first, cueArrow) {
			timingAt = 1
		}
		if timingAt >= len(block) || !strings.Contains(block[timingAt], cueArrow) {
			return nil, fmt.Errorf("%w: cue block without timing line: %q", ErrMalformed, first)
		}
		cue, err := parseTiming(block[timingAt])
		if err != nil {
			return nil, err
		}
		if timingAt == 1 {
			cue.ID = first
		}
		cue.Text = strings.Join(block[timingAt+1:], "\n")
		cues = append(cues, cue)
	}
	return cues, nil
}

func splitBlocks(text string) [][]string {
	var blocks [][]string
	var current []string
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			if len(current) > 0 {
				blocks = append(blocks, current)
				current = nil
			}
			continue
		}
		current = append(current, line)
	}
	if len(current) > 0 {
		blocks = append(blocks, current)
	}
	return blocks
}

func isSignature(line string) bool {
	if !strings.HasPrefix(line, signature) {
		return false
	}
	rest := line[len(signature):]
	return rest == "" || rest[0] == ' ' || rest[0] == '\t'
}

func isNonCueBlock(first string) bool {
	if strings.Contains(first, cueArrow) {
		return false
	}
	for _, prefix := range []string{"NOTE", "STYLE", "REGION"} {
		if first == prefix || strings.HasPrefix(first, prefix+" ") || strings.HasPrefix(first, prefix+"\t") {
			return true
		}
	}
	return false
}

func parseTiming(line string) (Cue, error) {
	left, right, _ := strings.Cut(line, cueArrow)
	start, err := parseTimestamp(strings.TrimSpace(left))
	if err != nil {
		return Cue{}, fmt.Errorf("%w: cue start in %q: %v", ErrMalformed, line, err)
	}
	right = strings.TrimSpace(right)
	endField, settings, _ := strings.Cut(right, " ")
	if before, after, found := strings.Cut(right, "\t"); found && len(before) < len(endField) {
		endField, settings = before, after
	}
	end, err := parseTimestamp(endField)
	if err != nil {
		return Cue{}, fmt.Errorf("%w: cue end in %q: %v", ErrMalformed, line, err)
	}
	return Cue{Start: start, End: end, Settings: strings.TrimSpace(settings)}, nil
}

func parseTimestamp(value string) (time.Duration, error) {
	main, fraction, found := strings.Cut(value, ".")
	if !found || len(fraction) != 3 {
		return 0, fmt.Errorf("invalid timestamp %q", value)
	}
	parts := strings.Split(main, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("invalid timestamp %q", value)
	}
	numbers := make([]int64, 0, 4)
	for _, part := range append(parts, fraction) {
		number, err := strconv.ParseInt(part, 10, 64)
		if err != nil || number < 0 {
			return 0, fmt.Errorf("invalid timestamp %q", value)
		}
		numbers = append(numbers, number)
	}
	var hours int64
	if len(parts) == 3 {
		hours = numbers[0]
		numbers = numbers[1:]
	}
	minutes, seconds, milliseconds := numbers[0], numbers[1], numbers[2]
	if minutes > 59 || seconds > 59 {
		return 0, fmt.Errorf("invalid timestamp %q", value)
	}
	total := ((hours*60+minutes)*60+seconds)*1000 + milliseconds
	return time.Duration(total) * time.Millisecond, nil
}

func cleanPayload(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		kept = append(kept, strings.ReplaceAll(line, cueArrow, neutralArrow))
	}
	return strings.Join(kept, "\n")
}

func singleLine(value string) string {
	value = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(value)
	return strings.TrimSpace(strings.ReplaceAll(value, cueArrow, neutralArrow))
}
