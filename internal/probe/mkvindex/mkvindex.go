package mkvindex

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"os"
	"sort"

	"github.com/at-wat/ebml-go"
)

var ErrUnsupported = errors.New("mkvindex: unsupported layout")

const (
	headWindow            = 64 << 10
	cueWindow             = 1 << 20
	maxHeadBytes          = 1 << 20
	maxCueBytes           = 16 << 20
	maxHeadElements       = 1 << 16
	maxCueElements        = 2000000
	defaultTimestampScale = 1000000
	elementIDLength       = 4
	maxSizeLength         = 8
	trackTypeVideo        = 1
	trackTypeAudio        = 2
	trackTypeSubtitle     = 0x11
	trackTypeMetadata     = 0x21
	maxCueGap             = 11.5
)

type cueIndex struct {
	Keyframes []float64
	Duration  float64
}

func (i *cueIndex) longestGap() float64 {
	longest := i.Duration - i.Keyframes[len(i.Keyframes)-1]
	for position := 1; position < len(i.Keyframes); position++ {
		longest = math.Max(longest, i.Keyframes[position]-i.Keyframes[position-1])
	}
	return longest
}

type header struct {
	EBML struct{} `ebml:"EBML,stop"`
}

type seekHead struct {
	SeekHead struct {
		Seek []struct {
			ID       []byte `ebml:"SeekID"`
			Position uint64 `ebml:"SeekPosition"`
		} `ebml:"Seek"`
	} `ebml:"SeekHead,stop"`
}

type info struct {
	Info struct {
		TimestampScale uint64  `ebml:"TimestampScale"`
		Duration       float64 `ebml:"Duration"`
	} `ebml:"Info,stop"`
}

type track struct {
	Number         uint64  `ebml:"TrackNumber"`
	Type           uint64  `ebml:"TrackType"`
	CodecID        string  `ebml:"CodecID"`
	CodecDelay     uint64  `ebml:"CodecDelay"`
	TimestampScale float64 `ebml:"TrackTimestampScale"`
}

type tracks struct {
	Tracks struct {
		Entries []track `ebml:"TrackEntry"`
	} `ebml:"Tracks,stop"`
}

type cues struct {
	Cues struct {
		Points []struct {
			Time      uint64 `ebml:"CueTime"`
			Positions []struct {
				Track uint64 `ebml:"CueTrack"`
			} `ebml:"CueTrackPositions"`
		} `ebml:"CuePoint"`
	} `ebml:"Cues,stop"`
}

func Keyframes(path string, streamIndex int) ([]float64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}
	return KeyframesFrom(file, stat.Size(), streamIndex)
}

func KeyframesFrom(r io.ReaderAt, size int64, streamIndex int) ([]float64, error) {
	index, err := readIndex(r, size, streamIndex)
	if err != nil {
		return nil, err
	}
	if gap := index.longestGap(); gap > maxCueGap {
		return nil, fmt.Errorf("%w: cue points leave a %.3f s gap", ErrUnsupported, gap)
	}
	return index.Keyframes, nil
}

type segmentLayout struct {
	cursor    *cursor
	start     int64
	positions map[ebml.ElementType]int64
}

func readLayout(r io.ReaderAt, size int64) (*segmentLayout, error) {
	c := &cursor{source: r, size: size}

	var head header
	if err := c.decode(&head, 0, headWindow, headWindow, maxHeadElements); err != nil {
		return nil, err
	}

	segment, declared, err := c.segmentStart()
	if err != nil {
		return nil, err
	}
	if declared >= 0 && declared > size-segment {
		return nil, fmt.Errorf("%w: segment declares %d bytes but the source holds %d", ErrUnsupported, declared, size-segment)
	}
	var seek seekHead
	if err := c.decode(&seek, segment, headWindow, headWindow, maxHeadElements); err != nil {
		return nil, err
	}
	positions := make(map[ebml.ElementType]int64)
	for _, entry := range seek.SeekHead.Seek {
		for _, element := range []ebml.ElementType{ebml.ElementInfo, ebml.ElementTracks, ebml.ElementCues, ebml.ElementTags} {
			if _, seen := positions[element]; !seen && bytes.Equal(entry.ID, element.Bytes()) {
				if entry.Position > uint64(size) {
					return nil, fmt.Errorf("%w: seek position past the end", ErrUnsupported)
				}
				positions[element] = segment + int64(entry.Position)
			}
		}
	}
	for _, element := range []ebml.ElementType{ebml.ElementInfo, ebml.ElementTracks, ebml.ElementCues} {
		if _, listed := positions[element]; !listed {
			return nil, fmt.Errorf("%w: seek head does not list info, tracks and cues", ErrUnsupported)
		}
	}
	return &segmentLayout{cursor: c, start: segment, positions: positions}, nil
}

func readIndex(r io.ReaderAt, size int64, streamIndex int) (*cueIndex, error) {
	layout, err := readLayout(r, size)
	if err != nil {
		return nil, err
	}
	c, positions := layout.cursor, layout.positions

	var segmentInfo info
	if err := c.decode(&segmentInfo, positions[ebml.ElementInfo], headWindow, maxHeadBytes, maxHeadElements); err != nil {
		return nil, err
	}
	scale := segmentInfo.Info.TimestampScale
	if scale == 0 {
		scale = defaultTimestampScale
	}
	duration := segmentInfo.Info.Duration * float64(scale) / 1e9
	if !(duration > 0) || math.IsInf(duration, 0) {
		return nil, fmt.Errorf("%w: segment declares no duration", ErrUnsupported)
	}

	var trackList tracks
	if err := c.decode(&trackList, positions[ebml.ElementTracks], headWindow, maxHeadBytes, maxHeadElements); err != nil {
		return nil, err
	}
	video, err := videoTrack(trackList.Tracks.Entries, streamIndex)
	if err != nil {
		return nil, err
	}

	var cueList cues
	if err := c.decode(&cueList, positions[ebml.ElementCues], cueWindow, maxCueBytes, maxCueElements); err != nil {
		return nil, err
	}
	delay := math.Round(float64(video.CodecDelay) / float64(scale))
	var keyframes []float64
	for _, point := range cueList.Cues.Points {
		for _, position := range point.Positions {
			if position.Track == video.Number {
				keyframes = append(keyframes, microseconds((float64(point.Time)-delay)*float64(scale)/1e9))
				break
			}
		}
	}
	if len(keyframes) == 0 {
		return nil, fmt.Errorf("%w: no cue points for the video track", ErrUnsupported)
	}
	sort.Float64s(keyframes)
	unique := keyframes[:1]
	for _, keyframe := range keyframes[1:] {
		if keyframe != unique[len(unique)-1] {
			unique = append(unique, keyframe)
		}
	}
	return &cueIndex{Keyframes: unique, Duration: duration}, nil
}

func microseconds(seconds float64) float64 {
	return math.Round(seconds*1e6) / 1e6
}

func videoTrack(entries []track, streamIndex int) (track, error) {
	position := 0
	for _, entry := range entries {
		switch entry.Type {
		case trackTypeVideo, trackTypeAudio, trackTypeSubtitle, trackTypeMetadata:
		default:
			continue
		}
		if entry.CodecID == "" {
			continue
		}
		if position != streamIndex {
			position++
			continue
		}
		if entry.Type != trackTypeVideo {
			return track{}, fmt.Errorf("%w: stream %d is not a video track", ErrUnsupported, streamIndex)
		}
		if entry.TimestampScale != 0 && entry.TimestampScale != 1 {
			return track{}, fmt.Errorf("%w: track timestamp scale %v", ErrUnsupported, entry.TimestampScale)
		}
		return entry, nil
	}
	return track{}, fmt.Errorf("%w: stream %d not found", ErrUnsupported, streamIndex)
}

type cursor struct {
	source io.ReaderAt
	size   int64
	pos    int64
	end    int64
	window int64
	start  int64
	buffer []byte
	budget int
	failed error
}

func (c *cursor) decode(target any, pos int64, window, limit int64, elements int) error {
	if pos < 0 || pos >= c.size {
		return fmt.Errorf("%w: element at %d is outside the file", ErrUnsupported, pos)
	}
	c.pos, c.window, c.end, c.budget, c.failed = pos, window, min(pos+limit, c.size), elements, nil
	err := ebml.Unmarshal(c, target,
		ebml.WithMaxLeafElementSize(uint64(c.end-pos)),
		ebml.WithElementReadHooks(func(*ebml.Element) { c.budget-- }),
	)
	if c.failed != nil {
		return c.failed
	}
	if c.budget < 0 {
		return fmt.Errorf("%w: element at %d holds more than %d entries", ErrUnsupported, pos, elements)
	}
	if !errors.Is(err, ebml.ErrReadStopped) {
		return fmt.Errorf("%w: element at %d: %v", ErrUnsupported, pos, err)
	}
	return nil
}

func (c *cursor) segmentStart() (int64, int64, error) {
	var head [elementIDLength + 1]byte
	c.end = min(c.pos+int64(len(head)), c.size)
	if _, err := io.ReadFull(c, head[:]); err != nil {
		if c.failed != nil {
			return 0, 0, c.failed
		}
		return 0, 0, fmt.Errorf("%w: truncated segment header", ErrUnsupported)
	}
	if !bytes.Equal(head[:elementIDLength], ebml.ElementSegment.Bytes()) {
		return 0, 0, fmt.Errorf("%w: no segment after the header", ErrUnsupported)
	}
	sizeLength := bits.LeadingZeros8(head[elementIDLength]) + 1
	if sizeLength > maxSizeLength {
		return 0, 0, fmt.Errorf("%w: invalid segment size", ErrUnsupported)
	}
	var rest [maxSizeLength - 1]byte
	c.end = min(c.pos+int64(sizeLength-1), c.size)
	if _, err := io.ReadFull(c, rest[:sizeLength-1]); err != nil {
		if c.failed != nil {
			return 0, 0, c.failed
		}
		return 0, 0, fmt.Errorf("%w: truncated segment header", ErrUnsupported)
	}
	valueBits := uint(7 * sizeLength)
	declared := uint64(head[elementIDLength]) & (0xff >> uint(sizeLength))
	for _, value := range rest[:sizeLength-1] {
		declared = declared<<8 | uint64(value)
	}
	if declared == 1<<valueBits-1 {
		return c.pos, -1, nil
	}
	return c.pos, int64(declared), nil
}

func (c *cursor) Read(p []byte) (int, error) {
	if c.failed != nil {
		return 0, c.failed
	}
	if c.pos >= c.end || c.budget < 0 {
		return 0, io.EOF
	}
	if c.pos < c.start || c.pos >= c.start+int64(len(c.buffer)) {
		want := min(c.window, c.size-c.pos)
		buffer := make([]byte, want)
		n, err := c.source.ReadAt(buffer, c.pos)
		if int64(n) < want {
			if err == nil || err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			c.failed = err
			return 0, err
		}
		c.start, c.buffer = c.pos, buffer
	}
	available := c.buffer[c.pos-c.start:]
	if remaining := c.end - c.pos; int64(len(available)) > remaining {
		available = available[:remaining]
	}
	n := copy(p, available)
	c.pos += int64(n)
	return n, nil
}
