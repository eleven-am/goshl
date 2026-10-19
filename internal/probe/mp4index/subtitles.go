package mp4index

import (
	"fmt"
	"io"
	"math"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
)

type SubtitleSample struct {
	Offset   int64
	Size     uint32
	Start    time.Duration
	Duration time.Duration
}

type SubtitleIndex struct {
	Codec   string
	Samples []SubtitleSample
}

func SubtitleIndexesFrom(r io.ReaderAt, size int64, streamIndexes []int) (map[int]SubtitleIndex, map[int]error, error) {
	moov, err := readMoov(r, size)
	if err != nil {
		return nil, nil, err
	}
	if moov.Mvex != nil || moov.Mvhd == nil || moov.Mvhd.Timescale == 0 {
		return nil, nil, fmt.Errorf("%w: fragmented or missing mvhd", ErrUnsupported)
	}
	indexes := make(map[int]SubtitleIndex, len(streamIndexes))
	failures := make(map[int]error)
	for _, streamIndex := range streamIndexes {
		if _, found := indexes[streamIndex]; found || failures[streamIndex] != nil {
			continue
		}
		if streamIndex < 0 || streamIndex >= len(moov.Traks) {
			failures[streamIndex] = fmt.Errorf("%w: no trak at index %d", ErrUnsupported, streamIndex)
			continue
		}
		index, err := trackSubtitleIndex(moov.Traks[streamIndex], uint64(moov.Mvhd.Timescale), size)
		if err != nil {
			failures[streamIndex] = err
			continue
		}
		indexes[streamIndex] = index
	}
	return indexes, failures, nil
}

func trackSubtitleIndex(trak *mp4.TrakBox, movieTimescale uint64, sourceSize int64) (SubtitleIndex, error) {
	if trak == nil || trak.Mdia == nil || trak.Mdia.Hdlr == nil || trak.Mdia.Mdhd == nil || trak.Mdia.Minf == nil || trak.Mdia.Minf.Stbl == nil {
		return SubtitleIndex{}, fmt.Errorf("%w: incomplete subtitle trak", ErrUnsupported)
	}
	stbl := trak.Mdia.Minf.Stbl
	if stbl.Stsd == nil || stbl.Stsc == nil || stbl.Stsz == nil || stbl.Stts == nil || stbl.Stco == nil && stbl.Co64 == nil {
		return SubtitleIndex{}, fmt.Errorf("%w: incomplete subtitle sample table", ErrUnsupported)
	}
	timescale := uint64(trak.Mdia.Mdhd.Timescale)
	if timescale == 0 {
		return SubtitleIndex{}, fmt.Errorf("%w: subtitle timescale is zero", ErrUnsupported)
	}
	sampleCount := stbl.Stsz.GetNrSamples()
	if sampleCount == 0 {
		return SubtitleIndex{Samples: []SubtitleSample{}}, nil
	}
	chunkCount := 0
	if stbl.Stco != nil {
		chunkCount = len(stbl.Stco.ChunkOffset)
	} else {
		chunkCount = len(stbl.Co64.ChunkOffset)
	}
	if chunkCount == 0 {
		return SubtitleIndex{}, fmt.Errorf("%w: subtitle track has no chunks", ErrUnsupported)
	}
	line, err := editTimeline(trak, movieTimescale, timescale)
	if err != nil {
		return SubtitleIndex{}, err
	}
	emptyTicks := (int64(line.emptyDur)*int64(timescale) + int64(movieTimescale)/2) / int64(movieTimescale)
	index := SubtitleIndex{Samples: make([]SubtitleSample, 0, sampleCount)}
	var nextSample uint32 = 1
	entryIndex := 0
	for chunkNumber := 1; chunkNumber <= chunkCount; chunkNumber++ {
		chunk, err := stbl.Stsc.GetChunk(uint32(chunkNumber))
		if err != nil {
			return SubtitleIndex{}, fmt.Errorf("%w: chunk %d: %v", ErrUnsupported, chunkNumber, err)
		}
		if chunk.StartSampleNr != nextSample || chunk.NrSamples == 0 {
			return SubtitleIndex{}, fmt.Errorf("%w: discontinuous subtitle chunks", ErrUnsupported)
		}
		for entryIndex+1 < len(stbl.Stsc.Entries) && uint32(chunkNumber) >= stbl.Stsc.Entries[entryIndex+1].FirstChunk {
			entryIndex++
		}
		descriptionID := stbl.Stsc.GetSampleDescriptionID(entryIndex + 1)
		if descriptionID == 0 {
			return SubtitleIndex{}, fmt.Errorf("%w: missing subtitle sample description", ErrUnsupported)
		}
		description, err := stbl.Stsd.GetSampleDescription(int(descriptionID - 1))
		if err != nil {
			return SubtitleIndex{}, fmt.Errorf("%w: subtitle sample description: %v", ErrUnsupported, err)
		}
		codec := description.Type()
		if codec != "tx3g" && codec != "wvtt" {
			return SubtitleIndex{}, fmt.Errorf("%w: subtitle sample entry %q", ErrUnsupported, codec)
		}
		if index.Codec == "" {
			index.Codec = codec
		} else if index.Codec != codec {
			return SubtitleIndex{}, fmt.Errorf("%w: mixed subtitle sample entries", ErrUnsupported)
		}
		offset, err := chunkOffset(stbl, chunkNumber)
		if err != nil {
			return SubtitleIndex{}, err
		}
		for inChunk := uint32(0); inChunk < chunk.NrSamples && nextSample <= sampleCount; inChunk++ {
			sampleSize := stbl.Stsz.GetSampleSize(int(nextSample))
			if offset > uint64(math.MaxInt64) || uint64(sampleSize) > uint64(sourceSize) || offset > uint64(sourceSize)-uint64(sampleSize) {
				return SubtitleIndex{}, fmt.Errorf("%w: subtitle sample %d is out of bounds", ErrUnsupported, nextSample)
			}
			decodeTime, duration, err := stbl.Stts.GetDecodeTime(nextSample)
			if err != nil || decodeTime > math.MaxInt64 {
				return SubtitleIndex{}, fmt.Errorf("%w: subtitle sample %d timing", ErrUnsupported, nextSample)
			}
			composition := int64(decodeTime)
			if stbl.Ctts != nil {
				composition += int64(stbl.Ctts.GetCompositionTimeOffset(nextSample))
			}
			startTicks := composition - line.mediaTime + emptyTicks
			endTicks := startTicks + int64(duration)
			if line.hasEditEnd {
				displayEnd := line.editEnd - line.mediaTime + emptyTicks
				if startTicks >= displayEnd {
					offset += uint64(sampleSize)
					nextSample++
					continue
				}
				if endTicks > displayEnd {
					endTicks = displayEnd
				}
			}
			if endTicks > 0 {
				if startTicks < 0 {
					startTicks = 0
				}
				index.Samples = append(index.Samples, SubtitleSample{
					Offset:   int64(offset),
					Size:     sampleSize,
					Start:    ticksDuration(startTicks, timescale),
					Duration: ticksDuration(endTicks-startTicks, timescale),
				})
			}
			offset += uint64(sampleSize)
			nextSample++
		}
	}
	if nextSample != sampleCount+1 {
		return SubtitleIndex{}, fmt.Errorf("%w: subtitle chunks cover %d of %d samples", ErrUnsupported, nextSample-1, sampleCount)
	}
	return index, nil
}

func chunkOffset(stbl *mp4.StblBox, chunkNumber int) (uint64, error) {
	if stbl.Stco != nil {
		offset, err := stbl.Stco.GetOffset(chunkNumber)
		if err != nil {
			return 0, fmt.Errorf("%w: subtitle chunk offset: %v", ErrUnsupported, err)
		}
		return offset, nil
	}
	offset, err := stbl.Co64.GetOffset(chunkNumber)
	if err != nil {
		return 0, fmt.Errorf("%w: subtitle chunk offset: %v", ErrUnsupported, err)
	}
	return offset, nil
}

func ticksDuration(ticks int64, timescale uint64) time.Duration {
	if ticks <= 0 {
		return 0
	}
	scale := int64(timescale)
	seconds := ticks / scale
	if seconds >= math.MaxInt64/int64(time.Second) {
		return time.Duration(math.MaxInt64)
	}
	remainder := ticks % scale
	return time.Duration(seconds*int64(time.Second) + remainder*int64(time.Second)/scale)
}
