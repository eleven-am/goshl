package subtitle

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/eleven-am/goshl/internal/mediasource"
	"github.com/eleven-am/goshl/internal/probe/mp4index"
	"github.com/eleven-am/goshl/internal/webvtt"
	"golang.org/x/sync/errgroup"
)

var ErrUnsupported = errors.New("indexed subtitle extraction is unsupported")

const maxSubtitleBytes = 64 << 20

const maxCoalesceGap = 256 << 10

const maxCoalescedSpan = 4 << 20

const maxCoalescedGapBytes = 16 << 20

type Track struct {
	ID          string
	StreamIndex int
	Codec       string
	Origin      time.Duration
}

type Outcome struct {
	Data      []byte
	Err       error
	Supported bool
}

type sampleRange struct {
	start   int64
	end     int64
	samples []sampleReference
}

type sampleReference struct {
	index  int
	offset int64
	size   int64
}

func ExtractBatch(ctx context.Context, sourceURL string, tracks []Track) (map[string]Outcome, error) {
	source, err := mediasource.Open(ctx, sourceURL)
	if err != nil {
		if errors.Is(err, mediasource.ErrUnsupported) {
			return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
		}
		return nil, err
	}
	defer source.Close()
	return extractBatchFrom(ctx, source, source.Size(), tracks)
}

func extractBatchFrom(ctx context.Context, source io.ReaderAt, sourceSize int64, tracks []Track) (map[string]Outcome, error) {
	streamIndexes := make([]int, 0, len(tracks))
	for _, track := range tracks {
		streamIndexes = append(streamIndexes, track.StreamIndex)
	}
	indexes, failures, err := mp4index.SubtitleIndexesFrom(ctx, source, sourceSize, streamIndexes)
	if err != nil {
		if errors.Is(err, mp4index.ErrUnsupported) {
			return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
		}
		return nil, err
	}

	outcomes := make(map[string]Outcome, len(tracks))
	for _, track := range tracks {
		if failure := failures[track.StreamIndex]; failure != nil {
			outcomes[track.ID] = Outcome{}
			continue
		}
		index, found := indexes[track.StreamIndex]
		if !found || !codecMatches(track.Codec, index.Codec) {
			outcomes[track.ID] = Outcome{}
			continue
		}
		data, err := extractTrack(ctx, source, index, track.Origin)
		outcomes[track.ID] = Outcome{Data: data, Err: err, Supported: true}
	}
	return outcomes, nil
}

func codecMatches(probed, indexed string) bool {
	switch indexed {
	case "tx3g":
		return probed == "" || probed == "mov_text" || probed == "tx3g"
	case "wvtt":
		return probed == "" || probed == "webvtt" || probed == "wvtt"
	default:
		return false
	}
}

func extractTrack(ctx context.Context, source io.ReaderAt, index mp4index.SubtitleIndex, origin time.Duration) ([]byte, error) {
	samples, err := readSamples(ctx, source, index.Samples)
	if err != nil {
		return nil, err
	}
	var cues []webvtt.Cue
	decoded, undecodable := 0, 0
	for sampleIndex, sample := range index.Samples {
		if sample.Duration <= 0 {
			continue
		}
		switch index.Codec {
		case "tx3g":
			text, err := decodeTX3G(samples[sampleIndex])
			if err != nil {
				undecodable++
				continue
			}
			decoded++
			if strings.TrimSpace(text) != "" {
				cues = append(cues, webvtt.Cue{Start: sample.Start, End: sample.Start + sample.Duration, Text: text})
			}
		case "wvtt":
			boxes, err := decodeWVTT(samples[sampleIndex])
			if err != nil {
				undecodable++
				continue
			}
			decoded++
			for _, cue := range boxes {
				cues = append(cues, webvtt.Cue{ID: cue.id, Start: sample.Start, End: sample.Start + sample.Duration, Settings: cue.settings, Text: cue.text})
			}
		default:
			return nil, ErrUnsupported
		}
	}
	if undecodable > 0 && decoded == 0 {
		return nil, fmt.Errorf("none of the %d %s subtitle samples could be decoded", undecodable, index.Codec)
	}
	return webvtt.Encode(webvtt.Shift(cues, origin), webvtt.Options{TimestampMap: true}), nil
}

func readSamples(ctx context.Context, source io.ReaderAt, samples []mp4index.SubtitleSample) ([][]byte, error) {
	data := make([][]byte, len(samples))
	references := make([]sampleReference, 0, len(samples))
	var total int64
	for index, sample := range samples {
		if sample.Size == 0 {
			data[index] = []byte{}
			continue
		}
		total += int64(sample.Size)
		if total > maxSubtitleBytes {
			return nil, fmt.Errorf("subtitle samples exceed %d bytes", maxSubtitleBytes)
		}
		references = append(references, sampleReference{index: index, offset: sample.Offset, size: int64(sample.Size)})
	}
	sort.Slice(references, func(i, j int) bool {
		return references[i].offset < references[j].offset
	})
	ranges := make([]sampleRange, 0, len(references))
	var gapBytes int64
	for _, reference := range references {
		gap := int64(0)
		if len(ranges) > 0 {
			gap = max(reference.offset-ranges[len(ranges)-1].end, 0)
		}
		if len(ranges) == 0 || gap > maxCoalesceGap || gapBytes+gap > maxCoalescedGapBytes || reference.offset+reference.size-ranges[len(ranges)-1].start > maxCoalescedSpan {
			ranges = append(ranges, sampleRange{start: reference.offset, end: reference.offset + reference.size, samples: []sampleReference{reference}})
			continue
		}
		gapBytes += gap
		last := &ranges[len(ranges)-1]
		last.end = max(last.end, reference.offset+reference.size)
		last.samples = append(last.samples, reference)
	}
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(8)
	for _, byteRange := range ranges {
		byteRange := byteRange
		group.Go(func() error {
			if err := groupCtx.Err(); err != nil {
				return err
			}
			buffer := make([]byte, byteRange.end-byteRange.start)
			if _, err := source.ReadAt(buffer, byteRange.start); err != nil {
				return fmt.Errorf("read subtitle bytes %d-%d: %w", byteRange.start, byteRange.end-1, err)
			}
			for _, reference := range byteRange.samples {
				start := reference.offset - byteRange.start
				data[reference.index] = append([]byte(nil), buffer[start:start+reference.size]...)
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	return data, nil
}

func decodeTX3G(data []byte) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	if len(data) < 2 {
		return "", fmt.Errorf("sample is shorter than its text length")
	}
	length := int(binary.BigEndian.Uint16(data[:2]))
	if length == 0 {
		return "", nil
	}
	if length > len(data)-2 {
		return "", fmt.Errorf("text length %d exceeds sample size %d", length, len(data))
	}
	encoded := data[2 : 2+length]
	text := ""
	if len(encoded) >= 2 && (encoded[0] == 0xfe && encoded[1] == 0xff || encoded[0] == 0xff && encoded[1] == 0xfe) {
		if len(encoded)%2 != 0 {
			return "", fmt.Errorf("utf-16 text has an odd byte length")
		}
		units := make([]uint16, 0, len(encoded)/2-1)
		var order binary.ByteOrder = binary.BigEndian
		if encoded[0] == 0xff {
			order = binary.LittleEndian
		}
		for position := 2; position < len(encoded); position += 2 {
			units = append(units, order.Uint16(encoded[position:position+2]))
		}
		text = string(utf16.Decode(units))
	} else {
		text = string(encoded)
	}
	text = strings.TrimPrefix(text, "\uFEFF")
	text = strings.ReplaceAll(text, "\x00", "")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return webvtt.EscapeText(text), nil
}

type webVTTCue struct {
	id       string
	settings string
	text     string
}

func decodeWVTT(data []byte) ([]webVTTCue, error) {
	var cues []webVTTCue
	for position := 0; position < len(data); {
		box, err := mp4.DecodeBox(uint64(position), bytes.NewReader(data[position:]))
		if err != nil {
			return nil, err
		}
		size := int(box.Size())
		if size <= 0 || position+size > len(data) {
			return nil, fmt.Errorf("invalid wvtt box size %d", size)
		}
		if cueBox, ok := box.(*mp4.VttcBox); ok && cueBox.Payl != nil {
			cue := webVTTCue{text: cueBox.Payl.CueText}
			if cueBox.Iden != nil {
				cue.id = cueBox.Iden.CueID
			}
			if cueBox.Sttg != nil {
				cue.settings = cueBox.Sttg.Settings
			}
			cues = append(cues, cue)
		}
		position += size
	}
	return cues, nil
}
