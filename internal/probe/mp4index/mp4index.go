package mp4index

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/Eyevinn/mp4ff/bits"
	"github.com/Eyevinn/mp4ff/mp4"
)

var ErrUnsupported = errors.New("mp4index: unsupported layout")

const (
	maxMoovBytes      = 256 << 20
	maxIndexedSamples = 1 << 25
	maxKeyframes      = 1 << 21
	maxTrailingBoxes  = 64
	smallHeader       = 8
	largeHeader       = 16
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

type Index struct {
	Keyframes  []float64
	GroupBytes []int64
}

func KeyframesFrom(r io.ReaderAt, size int64, streamIndex int) ([]float64, error) {
	index, err := indexFrom(r, size, streamIndex, false)
	return index.Keyframes, err
}

func IndexFrom(r io.ReaderAt, size int64, streamIndex int) (Index, error) {
	return indexFrom(r, size, streamIndex, true)
}

func indexFrom(r io.ReaderAt, size int64, streamIndex int, withBytes bool) (Index, error) {
	moov, err := readMoov(r, size)
	if err != nil {
		return Index{}, err
	}
	if moov.Mvex != nil || moov.Mvhd == nil || moov.Mvhd.Timescale == 0 {
		return Index{}, fmt.Errorf("%w: fragmented or missing mvhd", ErrUnsupported)
	}
	if streamIndex < 0 || streamIndex >= len(moov.Traks) {
		return Index{}, fmt.Errorf("%w: no trak at index %d", ErrUnsupported, streamIndex)
	}
	trak := moov.Traks[streamIndex]
	keyframes, err := trackKeyframes(trak, uint64(moov.Mvhd.Timescale), size)
	if err != nil {
		return Index{}, err
	}
	index := Index{Keyframes: keyframes}
	if withBytes {
		stbl := trak.Mdia.Minf.Stbl
		index.GroupBytes = groupBytes(stbl.Stsz, syncSampleNumbers(stbl.Stss, stbl.Stsz.GetNrSamples()))
	}
	return index, nil
}

func groupBytes(stsz *mp4.StszBox, syncSamples []uint32) []int64 {
	count := stsz.GetNrSamples()
	groups := make([]int64, len(syncSamples))
	if len(syncSamples) > 0 {
		for sample := uint32(1); sample < syncSamples[0] && sample <= count; sample++ {
			groups[0] += sampleBytes(stsz, sample)
		}
	}
	for position, first := range syncSamples {
		last := count
		if position+1 < len(syncSamples) {
			last = syncSamples[position+1] - 1
		}
		for sample := first; sample >= 1 && sample <= last; sample++ {
			groups[position] += sampleBytes(stsz, sample)
		}
	}
	return groups
}

func sampleBytes(stsz *mp4.StszBox, sample uint32) int64 {
	if stsz.SampleUniformSize != 0 {
		return int64(stsz.SampleUniformSize)
	}
	if int(sample) > len(stsz.SampleSize) {
		return 0
	}
	return int64(stsz.SampleSize[sample-1])
}

func plausibleSampleCount(stsz *mp4.StszBox, size int64) bool {
	count := uint64(stsz.GetNrSamples())
	if count > maxIndexedSamples {
		return false
	}
	return stsz.SampleUniformSize == 0 || len(stsz.SampleSize) != 0 || count*uint64(stsz.SampleUniformSize) <= uint64(size)
}

func readMoov(r io.ReaderAt, size int64) (*mp4.MoovBox, error) {
	for pos := int64(0); pos < size; {
		name, boxSize, err := boxHeader(r, pos, size)
		if err != nil {
			return nil, err
		}
		switch name {
		case "moof":
			return nil, fmt.Errorf("%w: fragmented", ErrUnsupported)
		case "moov":
			moov, err := decodeMoov(r, pos, boxSize, size)
			if err != nil {
				return nil, err
			}
			if err := requireCompleteBoxes(r, pos+boxSize, size); err != nil {
				return nil, err
			}
			return moov, nil
		}
		if boxSize > size-pos {
			break
		}
		pos += boxSize
	}
	return nil, fmt.Errorf("%w: no moov box", ErrUnsupported)
}

func requireCompleteBoxes(r io.ReaderAt, pos, size int64) error {
	for scanned := 0; pos < size && scanned < maxTrailingBoxes; scanned++ {
		_, boxSize, err := boxHeader(r, pos, size)
		if err != nil {
			return err
		}
		if boxSize > size-pos {
			return fmt.Errorf("%w: box at %d extends %d bytes past the end of the source", ErrUnsupported, pos, boxSize-(size-pos))
		}
		pos += boxSize
	}
	return nil
}

func boxHeader(r io.ReaderAt, pos, size int64) (string, int64, error) {
	var raw [largeHeader]byte
	want := min(int64(largeHeader), size-pos)
	n, err := r.ReadAt(raw[:want], pos)
	if int64(n) < want && err != nil && err != io.EOF {
		return "", 0, fmt.Errorf("mp4index: read box header at %d: %w", pos, err)
	}
	if n < smallHeader {
		return "", 0, fmt.Errorf("%w: truncated box header at %d", ErrUnsupported, pos)
	}
	name := string(raw[4:smallHeader])
	boxSize := int64(binary.BigEndian.Uint32(raw[0:4]))
	headerLength := int64(smallHeader)
	switch boxSize {
	case 0:
		boxSize = size - pos
	case 1:
		large := binary.BigEndian.Uint64(raw[smallHeader:largeHeader])
		if n < largeHeader || large > math.MaxInt64 {
			return "", 0, fmt.Errorf("%w: invalid large box size at %d", ErrUnsupported, pos)
		}
		boxSize = int64(large)
		headerLength = largeHeader
	}
	if boxSize < headerLength {
		return "", 0, fmt.Errorf("%w: invalid box size at %d", ErrUnsupported, pos)
	}
	return name, boxSize, nil
}

func decodeMoov(r io.ReaderAt, pos, boxSize, size int64) (*mp4.MoovBox, error) {
	if boxSize > maxMoovBytes || boxSize > size-pos {
		return nil, fmt.Errorf("%w: moov size %d", ErrUnsupported, boxSize)
	}
	data := make([]byte, boxSize)
	n, err := r.ReadAt(data, pos)
	if n < len(data) {
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("mp4index: read moov at %d: %w", pos, err)
		}
		return nil, fmt.Errorf("%w: truncated moov", ErrUnsupported)
	}
	if binary.BigEndian.Uint32(data[0:4]) == 0 {
		binary.BigEndian.PutUint32(data[0:4], uint32(boxSize))
	}
	box, err := mp4.DecodeBoxSR(uint64(pos), bits.NewFixedSliceReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	moov, ok := box.(*mp4.MoovBox)
	if !ok {
		return nil, fmt.Errorf("%w: moov decode", ErrUnsupported)
	}
	return moov, nil
}

type timeline struct {
	mediaTime  int64
	hasEdit    bool
	emptyDur   uint64
	editEnd    int64
	hasEditEnd bool
}

func trackKeyframes(trak *mp4.TrakBox, movieTimescale uint64, size int64) ([]float64, error) {
	if trak == nil || trak.Mdia == nil || trak.Mdia.Hdlr == nil || trak.Mdia.Mdhd == nil || trak.Mdia.Minf == nil || trak.Mdia.Minf.Stbl == nil {
		return nil, fmt.Errorf("%w: incomplete trak", ErrUnsupported)
	}
	if trak.Mdia.Hdlr.HandlerType != "vide" {
		return nil, fmt.Errorf("%w: handler %q is not video", ErrUnsupported, trak.Mdia.Hdlr.HandlerType)
	}
	mediaTimescale := uint64(trak.Mdia.Mdhd.Timescale)
	stbl := trak.Mdia.Minf.Stbl
	if mediaTimescale == 0 || stbl.Stts == nil || stbl.Stsz == nil {
		return nil, fmt.Errorf("%w: missing sample tables", ErrUnsupported)
	}
	if !plausibleSampleCount(stbl.Stsz, size) {
		return nil, fmt.Errorf("%w: sample count %d cannot fit in a %d byte source", ErrUnsupported, stbl.Stsz.GetNrSamples(), size)
	}
	sampleCount := stbl.Stsz.GetNrSamples()
	syncCount := uint64(sampleCount)
	if stbl.Stss != nil {
		syncCount = uint64(len(stbl.Stss.SampleNumber))
	}
	if sampleCount == 0 || syncCount == 0 {
		return nil, fmt.Errorf("%w: no samples or sync table", ErrUnsupported)
	}
	if syncCount > maxKeyframes {
		return nil, fmt.Errorf("%w: %d sync samples exceed the index limit", ErrUnsupported, syncCount)
	}
	syncSamples := syncSampleNumbers(stbl.Stss, sampleCount)

	line, err := editTimeline(trak, movieTimescale, mediaTimescale)
	if err != nil {
		return nil, err
	}

	firstComposition := int64(0)
	if stbl.Ctts != nil {
		firstComposition = int64(stbl.Ctts.GetCompositionTimeOffset(1))
	}
	if line.hasEdit && firstComposition != line.mediaTime {
		return nil, fmt.Errorf("%w: edit start is not the first composition time", ErrUnsupported)
	}

	mediaEnd, err := mediaDuration(stbl.Stts)
	if err != nil {
		return nil, err
	}
	if line.hasEditEnd && line.editEnd < mediaEnd {
		return nil, fmt.Errorf("%w: edit list shorter than media", ErrUnsupported)
	}

	decodeTimes, err := syncDecodeTimes(stbl.Stts, syncSamples, sampleCount)
	if err != nil {
		return nil, err
	}

	emptyTicks := (int64(line.emptyDur)*int64(mediaTimescale) + int64(movieTimescale)/2) / int64(movieTimescale)
	keyframes := make([]float64, 0, len(decodeTimes))
	for i, dts := range decodeTimes {
		composition := int64(dts)
		if stbl.Ctts != nil {
			composition += int64(stbl.Ctts.GetCompositionTimeOffset(syncSamples[i]))
		}
		seconds := float64(composition-line.mediaTime+emptyTicks) / float64(mediaTimescale)
		keyframes = append(keyframes, math.Round(seconds*1e6)/1e6)
	}
	return keyframes, nil
}

func syncSampleNumbers(stss *mp4.StssBox, sampleCount uint32) []uint32 {
	if stss != nil {
		return stss.SampleNumber
	}
	all := make([]uint32, sampleCount)
	for index := range all {
		all[index] = uint32(index) + 1
	}
	return all
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
	line.hasEdit = true
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
