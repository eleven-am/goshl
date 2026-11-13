package mkvindex

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math/bits"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/at-wat/ebml-go"
	"golang.org/x/sync/errgroup"
)

const (
	clusterProbe        = 32
	blockWindow         = 4 << 10
	blockCoalesceGap    = 16 << 10
	maxBlockBytes       = 1 << 20
	maxSubtitleBytes    = 64 << 20
	frameCountTag       = "NUMBER_OF_FRAMES"
	subtitleReadWorkers = 8
	cueingWriter        = "Lavf"
	leadScanLimit       = 1 << 20
	maxTopLevelElements = 64
	elementHeadWindow   = 2 * maxSizeLength
	leadWalkWindow      = 4 << 10
)

type SubtitlePacket struct {
	Offset   int64
	Time     uint64
	Duration uint64
	Data     []byte
}

type subtitleEntry struct {
	Number       uint64  `ebml:"TrackNumber"`
	UID          uint64  `ebml:"TrackUID"`
	Type         uint64  `ebml:"TrackType"`
	CodecID      string  `ebml:"CodecID"`
	CodecPrivate []byte  `ebml:"CodecPrivate"`
	Scale        float64 `ebml:"TrackTimestampScale"`
	Encodings    *struct {
		Encoding []struct{} `ebml:"ContentEncoding"`
	} `ebml:"ContentEncodings"`
}

type subtitleTracks struct {
	Tracks struct {
		Entries []subtitleEntry `ebml:"TrackEntry"`
	} `ebml:"Tracks,stop"`
}

type subtitleCues struct {
	Cues struct {
		Points []struct {
			Time      uint64 `ebml:"CueTime"`
			Positions []struct {
				Track    uint64 `ebml:"CueTrack"`
				Cluster  uint64 `ebml:"CueClusterPosition"`
				Relative uint64 `ebml:"CueRelativePosition"`
				Duration uint64 `ebml:"CueDuration"`
			} `ebml:"CueTrackPositions"`
		} `ebml:"CuePoint"`
	} `ebml:"Cues,stop"`
}

type trackTags struct {
	Tags struct {
		Tag []struct {
			Targets struct {
				TrackUID []uint64 `ebml:"TagTrackUID"`
			} `ebml:"Targets"`
			Simple []struct {
				Name  string `ebml:"TagName"`
				Value string `ebml:"TagString"`
			} `ebml:"SimpleTag"`
		} `ebml:"Tag"`
	} `ebml:"Tags,stop"`
}

type muxer struct {
	Info struct {
		TimestampScale uint64 `ebml:"TimestampScale"`
		MuxingApp      string `ebml:"MuxingApp"`
	} `ebml:"Info,stop"`
}

type clusterHead struct {
	Cluster struct {
		Timestamp uint64 `ebml:"Timestamp,stop"`
	} `ebml:"Cluster"`
}

type blockElement struct {
	Group *struct {
		Block    ebml.Block `ebml:"Block"`
		Duration uint64     `ebml:"BlockDuration"`
	} `ebml:"BlockGroup,stop"`
	Simple *ebml.Block `ebml:"SimpleBlock,stop"`
}

type SubtitleCue struct {
	Time     uint64
	Duration uint64
	Cluster  int64
	Relative int64
}

type SubtitleLayout struct {
	MuxingApp      string
	TimestampScale uint64
	Tracks         map[int]SubtitleCandidate
	CuedFromStart  bool
}

type clusterLead struct {
	Timestamp uint64       `ebml:"Timestamp"`
	Simple    []ebml.Block `ebml:"SimpleBlock"`
	Groups    []struct {
		Block ebml.Block `ebml:"Block"`
	} `ebml:"BlockGroup"`
}

func (l *SubtitleLayout) Complete(stream int) bool {
	candidate, found := l.Tracks[stream]
	if !found {
		return false
	}
	if l.CuedFromStart {
		return candidate.Complete(l.MuxingApp)
	}
	return candidate.Complete("")
}

func elementHead(data []byte) (uint32, int64, int64, bool) {
	if len(data) == 0 || data[0] == 0 {
		return 0, 0, 0, false
	}
	idLength := bits.LeadingZeros8(data[0]) + 1
	if idLength > elementIDLength || len(data) <= idLength || data[idLength] == 0 {
		return 0, 0, 0, false
	}
	var id uint32
	for _, value := range data[:idLength] {
		id = id<<8 | uint32(value)
	}
	sizeLength := bits.LeadingZeros8(data[idLength]) + 1
	if len(data) < idLength+sizeLength {
		return 0, 0, 0, false
	}
	length := uint64(data[idLength]) & (0xff >> uint(sizeLength))
	for _, value := range data[idLength+1 : idLength+sizeLength] {
		length = length<<8 | uint64(value)
	}
	if length == 1<<uint(7*sizeLength)-1 {
		return id, int64(idLength + sizeLength), -1, true
	}
	return id, int64(idLength + sizeLength), int64(length), true
}

func (c *cursor) peek(position, length int64) []byte {
	c.pos, c.window, c.end, c.budget, c.failed = position, leadWalkWindow, min(position+length, c.size), 0, nil
	head := make([]byte, c.end-position)
	if _, err := io.ReadFull(c, head); err != nil {
		return nil
	}
	return head
}

func firstCluster(c *cursor, segment int64) (int64, int64, bool) {
	clusterID := binary.BigEndian.Uint32(ebml.ElementCluster.Bytes())
	size := c.size
	position := segment
	for scanned := 0; scanned < maxTopLevelElements && position < size; scanned++ {
		id, headLength, length, ok := elementHead(c.peek(position, elementHeadWindow))
		if !ok {
			return 0, 0, false
		}
		if id == clusterID {
			return position, position + headLength, true
		}
		if length < 0 || length > size-position-headLength {
			return 0, 0, false
		}
		position += headLength + length
	}
	return 0, 0, false
}

func cuedFromStart(r io.ReaderAt, size, cluster, data int64, cues map[uint64][]SubtitleCue) bool {
	lead := int64(0)
	for _, list := range cues {
		for _, cue := range list {
			if cue.Cluster == cluster && cue.Relative > 0 && (lead == 0 || cue.Relative < lead) {
				lead = cue.Relative
			}
		}
	}
	if lead == 0 || lead > leadScanLimit {
		return false
	}
	buffer, err := readWindow(r, size, data, lead)
	if err != nil || int64(len(buffer)) != lead {
		return false
	}
	var blocks clusterLead
	if err := ebml.Unmarshal(bytes.NewReader(buffer), &blocks, ebml.WithIgnoreUnknown(true), ebml.WithMaxLeafElementSize(uint64(lead))); err != nil && err != io.EOF {
		return false
	}
	for _, block := range blocks.Simple {
		if int64(blocks.Timestamp)+int64(block.Timecode) < 0 {
			return false
		}
	}
	for _, group := range blocks.Groups {
		if int64(blocks.Timestamp)+int64(group.Block.Timecode) < 0 {
			return false
		}
	}
	return true
}

type SubtitleCandidate struct {
	Number       uint64
	CodecID      string
	CodecPrivate []byte
	Compressed   bool
	FrameCount   int
	Cues         []SubtitleCue
}

func ReadSubtitleLayout(r io.ReaderAt, size int64) (*SubtitleLayout, error) {
	segmentLayout, err := readLayout(r, size)
	if err != nil {
		return nil, err
	}
	c, segment, positions := segmentLayout.cursor, segmentLayout.start, segmentLayout.positions
	var segmentInfo muxer
	if err := c.decode(&segmentInfo, positions[ebml.ElementInfo], headWindow, maxHeadBytes, maxHeadElements); err != nil {
		return nil, err
	}
	layout := &SubtitleLayout{MuxingApp: segmentInfo.Info.MuxingApp, TimestampScale: segmentInfo.Info.TimestampScale, Tracks: make(map[int]SubtitleCandidate)}
	if layout.TimestampScale == 0 {
		layout.TimestampScale = defaultTimestampScale
	}
	var trackList subtitleTracks
	if err := c.decode(&trackList, positions[ebml.ElementTracks], headWindow, maxHeadBytes, maxHeadElements); err != nil {
		return nil, err
	}
	frames := make(map[uint64]int)
	if at, found := positions[ebml.ElementTags]; found {
		var tags trackTags
		if err := c.decode(&tags, at, headWindow, maxHeadBytes, maxHeadElements); err != nil {
			return nil, err
		}
		for _, tag := range tags.Tags.Tag {
			for _, simple := range tag.Simple {
				if simple.Name != frameCountTag && !strings.HasPrefix(simple.Name, frameCountTag+"-") {
					continue
				}
				count, err := strconv.Atoi(strings.TrimSpace(simple.Value))
				if err != nil || count < 0 {
					continue
				}
				for _, uid := range tag.Targets.TrackUID {
					frames[uid] = count
				}
			}
		}
	}
	cluster, clusterData, clusterFound := int64(0), int64(0), false
	if strings.HasPrefix(layout.MuxingApp, cueingWriter) {
		cluster, clusterData, clusterFound = firstCluster(c, segment)
	}
	var cueList subtitleCues
	if err := c.decode(&cueList, positions[ebml.ElementCues], cueWindow, maxCueBytes, maxCueElements); err != nil {
		return nil, err
	}
	byNumber := make(map[uint64][]SubtitleCue)
	for _, point := range cueList.Cues.Points {
		for _, position := range point.Positions {
			if position.Cluster > uint64(size) || position.Relative > uint64(size) {
				return nil, fmt.Errorf("%w: cue position past the end", ErrUnsupported)
			}
			byNumber[position.Track] = append(byNumber[position.Track], SubtitleCue{Time: point.Time, Duration: position.Duration, Cluster: segment + int64(position.Cluster), Relative: int64(position.Relative)})
		}
	}
	layout.CuedFromStart = clusterFound && cuedFromStart(r, size, cluster, clusterData, byNumber)
	stream := 0
	for _, entry := range trackList.Tracks.Entries {
		switch entry.Type {
		case trackTypeVideo, trackTypeAudio, trackTypeSubtitle, trackTypeMetadata:
		default:
			continue
		}
		if entry.CodecID == "" {
			continue
		}
		if entry.Type == trackTypeSubtitle && (entry.Scale == 0 || entry.Scale == 1) {
			count, counted := frames[entry.UID]
			if !counted {
				count = -1
			}
			layout.Tracks[stream] = SubtitleCandidate{Number: entry.Number, CodecID: entry.CodecID, CodecPrivate: entry.CodecPrivate, Compressed: entry.Encodings != nil, FrameCount: count, Cues: byNumber[entry.Number]}
		}
		stream++
	}
	return layout, nil
}

func (c SubtitleCandidate) Complete(muxingApp string) bool {
	if c.Compressed || len(c.Cues) == 0 {
		return false
	}
	for _, cue := range c.Cues {
		if cue.Relative == 0 {
			return false
		}
	}
	return c.FrameCount == len(c.Cues) || strings.HasPrefix(muxingApp, cueingWriter)
}

type blockReference struct {
	stream int
	index  int
	offset int64
	base   uint64
	cue    SubtitleCue
}

type blockRange struct {
	start  int64
	end    int64
	blocks []blockReference
}

func ReadSubtitlePackets(ctx context.Context, r io.ReaderAt, size int64, tracks map[int]SubtitleCandidate) (map[int][]SubtitlePacket, error) {
	type clusterInfo struct {
		data      int64
		timestamp uint64
	}
	clusters := make(map[int64]*clusterInfo)
	for _, track := range tracks {
		for _, cue := range track.Cues {
			clusters[cue.Cluster] = &clusterInfo{}
		}
	}
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(subtitleReadWorkers)
	for at, cluster := range clusters {
		group.Go(func() error {
			if err := groupCtx.Err(); err != nil {
				return err
			}
			head, err := readWindow(r, size, at, clusterProbe)
			if err != nil {
				return err
			}
			if len(head) <= elementIDLength || !bytes.Equal(head[:elementIDLength], ebml.ElementCluster.Bytes()) {
				return nil
			}
			var decoded clusterHead
			if err := ebml.Unmarshal(bytes.NewReader(head), &decoded); err != ebml.ErrReadStopped {
				return nil
			}
			cluster.data = at + elementIDLength + int64(bits.LeadingZeros8(head[elementIDLength])+1)
			cluster.timestamp = decoded.Cluster.Timestamp
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}

	var references []blockReference
	packets := make(map[int][]SubtitlePacket, len(tracks))
	broken := make(map[int]bool)
	for stream, track := range tracks {
		packets[stream] = make([]SubtitlePacket, len(track.Cues))
		for index, cue := range track.Cues {
			cluster := clusters[cue.Cluster]
			if cluster.data == 0 {
				broken[stream] = true
				break
			}
			references = append(references, blockReference{stream: stream, index: index, offset: cluster.data + cue.Relative, base: cluster.timestamp, cue: cue})
		}
	}
	sort.Slice(references, func(i, j int) bool { return references[i].offset < references[j].offset })
	var ranges []blockRange
	var planned int64
	for _, reference := range references {
		if broken[reference.stream] {
			continue
		}
		last := len(ranges) - 1
		if last < 0 || reference.offset > ranges[last].end+blockCoalesceGap || reference.offset+blockWindow-ranges[last].start > maxBlockBytes {
			ranges = append(ranges, blockRange{start: reference.offset})
			last++
		}
		planned += reference.offset + blockWindow - max(ranges[last].end, ranges[last].start)
		ranges[last].end = reference.offset + blockWindow
		ranges[last].blocks = append(ranges[last].blocks, reference)
	}
	if planned > maxSubtitleBytes {
		return nil, fmt.Errorf("%w: subtitle blocks span more than %d bytes", ErrUnsupported, maxSubtitleBytes)
	}

	var lock sync.Mutex
	group, groupCtx = errgroup.WithContext(ctx)
	group.SetLimit(subtitleReadWorkers)
	for _, byteRange := range ranges {
		group.Go(func() error {
			if err := groupCtx.Err(); err != nil {
				return err
			}
			buffer, err := readWindow(r, size, byteRange.start, byteRange.end-byteRange.start)
			if err != nil {
				return err
			}
			for _, reference := range byteRange.blocks {
				data := buffer[min(reference.offset-byteRange.start, int64(len(buffer))):]
				packet, ok := decodeSubtitleBlock(data, tracks[reference.stream].Number, reference)
				if !ok && reference.offset+int64(len(data)) < size {
					if data, err = readWindow(r, size, reference.offset, maxBlockBytes); err != nil {
						return err
					}
					packet, ok = decodeSubtitleBlock(data, tracks[reference.stream].Number, reference)
				}
				lock.Lock()
				if ok {
					packets[reference.stream][reference.index] = packet
				} else {
					broken[reference.stream] = true
				}
				lock.Unlock()
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	for stream, list := range packets {
		if broken[stream] {
			delete(packets, stream)
			continue
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Offset < list[j].Offset })
	}
	return packets, nil
}

func decodeSubtitleBlock(data []byte, trackNumber uint64, reference blockReference) (SubtitlePacket, bool) {
	var element blockElement
	if err := ebml.Unmarshal(bytes.NewReader(data), &element, ebml.WithMaxLeafElementSize(uint64(len(data)))); err != ebml.ErrReadStopped {
		return SubtitlePacket{}, false
	}
	block, duration := element.Simple, uint64(0)
	if element.Group != nil {
		block, duration = &element.Group.Block, element.Group.Duration
	}
	if block == nil || block.TrackNumber != trackNumber || int64(reference.base)+int64(block.Timecode) != int64(reference.cue.Time) || len(block.Data) != 1 {
		return SubtitlePacket{}, false
	}
	return SubtitlePacket{Offset: reference.offset, Time: reference.cue.Time, Duration: duration, Data: block.Data[0]}, true
}

func readWindow(r io.ReaderAt, size, at, length int64) ([]byte, error) {
	if at < 0 || at >= size {
		return nil, fmt.Errorf("%w: position %d is outside the file", ErrUnsupported, at)
	}
	buffer := make([]byte, min(length, size-at))
	if n, err := r.ReadAt(buffer, at); n < len(buffer) {
		if err == nil || err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("read %d bytes at %d: %w", len(buffer), at, err)
	}
	return buffer, nil
}
