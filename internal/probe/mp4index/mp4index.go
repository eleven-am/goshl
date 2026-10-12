package mp4index

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/Eyevinn/mp4ff/mp4"
)

var ErrUnsupported = errors.New("mp4index: unsupported layout")

const (
	maxMoovBytes = 256 << 20
	headerProbe  = 16
)

func Keyframes(path string, streamIndex int) ([]float64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	return KeyframesFrom(file, info.Size(), streamIndex)
}

func KeyframesFrom(r io.ReaderAt, size int64, streamIndex int) ([]float64, error) {
	moov, err := readMoov(r, size)
	if err != nil {
		return nil, err
	}
	if moov.Mvex != nil || moov.Mvhd == nil || moov.Mvhd.Timescale == 0 {
		return nil, fmt.Errorf("%w: fragmented or missing mvhd", ErrUnsupported)
	}
	if streamIndex < 0 || streamIndex >= len(moov.Traks) {
		return nil, fmt.Errorf("%w: no trak at index %d", ErrUnsupported, streamIndex)
	}
	return trackKeyframes(moov.Traks[streamIndex], uint64(moov.Mvhd.Timescale))
}

func readMoov(r io.ReaderAt, size int64) (*mp4.MoovBox, error) {
	for pos := int64(0); pos < size; {
		header, err := mp4.DecodeHeader(io.NewSectionReader(r, pos, headerProbe))
		if err != nil {
			return nil, fmt.Errorf("%w: box header at %d: %v", ErrUnsupported, pos, err)
		}
		boxSize := int64(header.Size)
		if boxSize < int64(header.Hdrlen) || boxSize < 0 {
			return nil, fmt.Errorf("%w: invalid box size at %d", ErrUnsupported, pos)
		}
		switch header.Name {
		case "moof":
			return nil, fmt.Errorf("%w: fragmented", ErrUnsupported)
		case "moov":
			if boxSize > maxMoovBytes || pos+boxSize > size {
				return nil, fmt.Errorf("%w: moov size %d", ErrUnsupported, boxSize)
			}
			box, err := mp4.DecodeBox(uint64(pos), io.NewSectionReader(r, pos, boxSize))
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
			}
			moov, ok := box.(*mp4.MoovBox)
			if !ok {
				return nil, fmt.Errorf("%w: moov decode", ErrUnsupported)
			}
			return moov, nil
		}
		pos += boxSize
	}
	return nil, fmt.Errorf("%w: no moov box", ErrUnsupported)
}

type timeline struct {
	mediaTime  int64
	emptyDur   uint64
	editEnd    int64
	hasEditEnd bool
}

func trackKeyframes(trak *mp4.TrakBox, movieTimescale uint64) ([]float64, error) {
	if trak == nil || trak.Mdia == nil || trak.Mdia.Hdlr == nil || trak.Mdia.Mdhd == nil || trak.Mdia.Minf == nil || trak.Mdia.Minf.Stbl == nil {
		return nil, fmt.Errorf("%w: incomplete trak", ErrUnsupported)
	}
	if trak.Mdia.Hdlr.HandlerType != "vide" {
		return nil, fmt.Errorf("%w: handler %q is not video", ErrUnsupported, trak.Mdia.Hdlr.HandlerType)
	}
	mediaTimescale := uint64(trak.Mdia.Mdhd.Timescale)
	stbl := trak.Mdia.Minf.Stbl
	if mediaTimescale == 0 || stbl.Stts == nil || stbl.Stsz == nil || stbl.Stss == nil {
		return nil, fmt.Errorf("%w: missing sample tables", ErrUnsupported)
	}
	sampleCount := stbl.Stsz.GetNrSamples()
	if sampleCount == 0 || len(stbl.Stss.SampleNumber) == 0 {
		return nil, fmt.Errorf("%w: no samples or sync table", ErrUnsupported)
	}

	line, err := editTimeline(trak, movieTimescale, mediaTimescale)
	if err != nil {
		return nil, err
	}

	if line.mediaTime != 0 && (stbl.Ctts == nil || int64(stbl.Ctts.GetCompositionTimeOffset(1)) != line.mediaTime) {
		return nil, fmt.Errorf("%w: edit start is not the first composition time", ErrUnsupported)
	}

	mediaEnd, err := mediaDuration(stbl.Stts)
	if err != nil {
		return nil, err
	}
	if line.hasEditEnd && line.editEnd < mediaEnd {
		return nil, fmt.Errorf("%w: edit list shorter than media", ErrUnsupported)
	}

	decodeTimes, err := syncDecodeTimes(stbl.Stts, stbl.Stss.SampleNumber, sampleCount)
	if err != nil {
		return nil, err
	}

	emptyTicks := (int64(line.emptyDur)*int64(mediaTimescale) + int64(movieTimescale)/2) / int64(movieTimescale)
	keyframes := make([]float64, 0, len(decodeTimes))
	for i, dts := range decodeTimes {
		composition := int64(dts)
		if stbl.Ctts != nil {
			composition += int64(stbl.Ctts.GetCompositionTimeOffset(stbl.Stss.SampleNumber[i]))
		}
		seconds := float64(composition-line.mediaTime+emptyTicks) / float64(mediaTimescale)
		keyframes = append(keyframes, math.Round(seconds*1e6)/1e6)
	}
	return keyframes, nil
}

func editTimeline(trak *mp4.TrakBox, movieTimescale, mediaTimescale uint64) (timeline, error) {
	var line timeline
	if trak.Edts == nil || trak.Edts.Elst == nil || len(trak.Edts.Elst) == 0 {
		return line, nil
	}
	entries := trak.Edts.Elst[0].Entries
	index := 0
	for index < len(entries) && entries[index].MediaTime == -1 {
		line.emptyDur += entries[index].SegmentDuration
		index++
	}
	if index == len(entries) {
		if len(entries) == 0 {
			return line, nil
		}
		return line, fmt.Errorf("%w: edit list has no media edit", ErrUnsupported)
	}
	if len(entries)-index != 1 || len(trak.Edts.Elst) != 1 {
		return line, fmt.Errorf("%w: complex edit list", ErrUnsupported)
	}
	edit := entries[index]
	if edit.MediaTime < 0 || edit.MediaRateInteger != 1 || edit.MediaRateFraction != 0 {
		return line, fmt.Errorf("%w: unsupported edit rate or time", ErrUnsupported)
	}
	line.mediaTime = edit.MediaTime
	if edit.SegmentDuration > 0 {
		line.hasEditEnd = true
		scaled := edit.SegmentDuration * mediaTimescale / movieTimescale
		line.editEnd = edit.MediaTime + int64(scaled) + int64(mediaTimescale/movieTimescale) + 1
	}
	return line, nil
}

func mediaDuration(stts *mp4.SttsBox) (int64, error) {
	var total int64
	for i := range stts.SampleCount {
		total += int64(stts.SampleCount[i]) * int64(stts.SampleTimeDelta[i])
	}
	if total <= 0 {
		return 0, fmt.Errorf("%w: empty stts", ErrUnsupported)
	}
	return total, nil
}

func syncDecodeTimes(stts *mp4.SttsBox, syncSamples []uint32, sampleCount uint32) ([]uint64, error) {
	result := make([]uint64, 0, len(syncSamples))
	var (
		run       int
		runStart  uint32 = 1
		runDecode uint64
		previous  uint32
	)
	for _, sample := range syncSamples {
		if sample == 0 || sample > sampleCount || sample <= previous {
			return nil, fmt.Errorf("%w: invalid sync sample %d", ErrUnsupported, sample)
		}
		previous = sample
		for run < len(stts.SampleCount) && sample >= runStart+stts.SampleCount[run] {
			runDecode += uint64(stts.SampleCount[run]) * uint64(stts.SampleTimeDelta[run])
			runStart += stts.SampleCount[run]
			run++
		}
		if run >= len(stts.SampleCount) {
			return nil, fmt.Errorf("%w: sync sample %d beyond stts", ErrUnsupported, sample)
		}
		result = append(result, runDecode+uint64(sample-runStart)*uint64(stts.SampleTimeDelta[run]))
	}
	return result, nil
}
