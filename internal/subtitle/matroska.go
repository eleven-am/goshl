package subtitle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/at-wat/ebml-go"
	"github.com/eleven-am/goshl/internal/mediasource"
	"github.com/eleven-am/goshl/internal/probe/mkvindex"
)

const (
	compactClusterSpan   = 30000
	compactClusterBlocks = 256
	matroskaSubtitleType = 0x11
)

var matroskaTextCodecs = map[string][]string{
	"S_TEXT/UTF8": {"subrip", "srt"},
	"S_TEXT/ASS":  {"ass", "ssa"},
	"S_TEXT/SSA":  {"ass", "ssa"},
}

type Compact struct {
	Path    string
	Streams map[string]int
}

type compactFile struct {
	Header struct {
		Version            uint64 `ebml:"EBMLVersion"`
		ReadVersion        uint64 `ebml:"EBMLReadVersion"`
		MaxIDLength        uint64 `ebml:"EBMLMaxIDLength"`
		MaxSizeLength      uint64 `ebml:"EBMLMaxSizeLength"`
		DocType            string `ebml:"EBMLDocType"`
		DocTypeVersion     uint64 `ebml:"EBMLDocTypeVersion"`
		DocTypeReadVersion uint64 `ebml:"EBMLDocTypeReadVersion"`
	} `ebml:"EBML"`
	Segment struct {
		Info struct {
			TimestampScale uint64 `ebml:"TimestampScale"`
			MuxingApp      string `ebml:"MuxingApp"`
			WritingApp     string `ebml:"WritingApp"`
		} `ebml:"Info"`
		Tracks struct {
			Entries []compactTrack `ebml:"TrackEntry"`
		} `ebml:"Tracks"`
		Clusters []compactCluster `ebml:"Cluster"`
	} `ebml:"Segment"`
}

type compactTrack struct {
	Number       uint64 `ebml:"TrackNumber"`
	UID          uint64 `ebml:"TrackUID"`
	Type         uint64 `ebml:"TrackType"`
	Lacing       uint64 `ebml:"FlagLacing"`
	CodecID      string `ebml:"CodecID"`
	CodecPrivate []byte `ebml:"CodecPrivate,omitempty"`
}

type compactCluster struct {
	Timestamp uint64         `ebml:"Timestamp"`
	Groups    []compactGroup `ebml:"BlockGroup"`
}

type compactGroup struct {
	Block    ebml.Block `ebml:"Block"`
	Duration uint64     `ebml:"BlockDuration,omitempty"`
}

type compactPacket struct {
	track  uint64
	packet mkvindex.SubtitlePacket
}

func CompactMatroska(ctx context.Context, sourceURL string, tracks []Track, dir string) (*Compact, error) {
	source, err := mediasource.Open(ctx, sourceURL)
	if err != nil {
		if errors.Is(err, mediasource.ErrUnsupported) {
			return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
		}
		return nil, err
	}
	defer source.Close()

	layout, err := mkvindex.ReadSubtitleLayout(source, source.Size())
	if err != nil {
		if errors.Is(err, mkvindex.ErrUnsupported) {
			return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
		}
		return nil, err
	}

	var file compactFile
	file.Header.Version, file.Header.ReadVersion = 1, 1
	file.Header.MaxIDLength, file.Header.MaxSizeLength = 4, 8
	file.Header.DocType, file.Header.DocTypeVersion, file.Header.DocTypeReadVersion = "matroska", 4, 2
	file.Segment.Info.TimestampScale = layout.TimestampScale
	file.Segment.Info.MuxingApp, file.Segment.Info.WritingApp = "goshl", "goshl"

	compact := &Compact{Path: filepath.Join(dir, "subtitles.mkv"), Streams: make(map[string]int)}
	candidates := make(map[int]mkvindex.SubtitleCandidate)
	for _, track := range tracks {
		candidate, found := layout.Tracks[track.StreamIndex]
		if found && matroskaCodecMatches(track.Codec, candidate.CodecID) && layout.Complete(track.StreamIndex) {
			candidates[track.StreamIndex] = candidate
		}
	}
	read, err := mkvindex.ReadSubtitlePackets(ctx, source, source.Size(), candidates)
	if err != nil {
		if errors.Is(err, mkvindex.ErrUnsupported) {
			return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
		}
		return nil, err
	}
	var packets []compactPacket
	for _, track := range tracks {
		list, found := read[track.StreamIndex]
		if _, seen := compact.Streams[track.ID]; !found || seen {
			continue
		}
		candidate := candidates[track.StreamIndex]
		number := uint64(len(file.Segment.Tracks.Entries) + 1)
		file.Segment.Tracks.Entries = append(file.Segment.Tracks.Entries, compactTrack{Number: number, UID: number, Type: matroskaSubtitleType, CodecID: candidate.CodecID, CodecPrivate: candidate.CodecPrivate})
		compact.Streams[track.ID] = int(number - 1)
		for _, packet := range list {
			packets = append(packets, compactPacket{track: number, packet: packet})
		}
	}
	if len(compact.Streams) == 0 {
		return nil, fmt.Errorf("%w: no subtitle track is fully indexed", ErrUnsupported)
	}

	sort.Slice(packets, func(i, j int) bool { return packets[i].packet.Offset < packets[j].packet.Offset })
	for _, entry := range packets {
		last := len(file.Segment.Clusters) - 1
		if last < 0 || entry.packet.Time < file.Segment.Clusters[last].Timestamp || entry.packet.Time-file.Segment.Clusters[last].Timestamp > compactClusterSpan || len(file.Segment.Clusters[last].Groups) >= compactClusterBlocks {
			file.Segment.Clusters = append(file.Segment.Clusters, compactCluster{Timestamp: entry.packet.Time})
			last++
		}
		cluster := &file.Segment.Clusters[last]
		cluster.Groups = append(cluster.Groups, compactGroup{
			Block:    ebml.Block{TrackNumber: entry.track, Timecode: int16(entry.packet.Time - cluster.Timestamp), Keyframe: true, Data: [][]byte{entry.packet.Data}},
			Duration: entry.packet.Duration,
		})
	}

	output, err := os.Create(compact.Path)
	if err != nil {
		return nil, fmt.Errorf("create compact subtitle source: %w", err)
	}
	if err := ebml.Marshal(&file, output); err != nil {
		output.Close()
		return nil, fmt.Errorf("write compact subtitle source: %w", err)
	}
	if err := output.Close(); err != nil {
		return nil, fmt.Errorf("write compact subtitle source: %w", err)
	}
	return compact, nil
}

func matroskaCodecMatches(probed, codecID string) bool {
	for _, name := range matroskaTextCodecs[codecID] {
		if strings.EqualFold(probed, name) {
			return true
		}
	}
	return false
}
