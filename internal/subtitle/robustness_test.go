package subtitle

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/eleven-am/goshl/internal/probe/mp4index"
)

func craftedSubtitleSource(t *testing.T, mutate func(*mp4.StblBox)) []byte {
	t.Helper()
	data, err := os.ReadFile(makeMovTextFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	start := bytes.Index(data, []byte("moov")) - 4
	if start < 0 {
		t.Fatal("fixture has no moov box")
	}
	size := int(binary.BigEndian.Uint32(data[start:]))
	box, err := mp4.DecodeBox(uint64(start), bytes.NewReader(data[start:start+size]))
	if err != nil {
		t.Fatal(err)
	}
	moov := box.(*mp4.MoovBox)
	mutate(moov.Traks[1].Mdia.Minf.Stbl)
	var out bytes.Buffer
	out.Write(data[:start])
	if err := moov.Encode(&out); err != nil {
		t.Fatal(err)
	}
	out.Write(make([]byte, 64<<10))
	return out.Bytes()
}

func floodChunks(stbl *mp4.StblBox, samples, perChunk uint32) {
	stbl.Stsz.SampleUniformSize = 1
	stbl.Stsz.SampleNumber = samples
	stbl.Stsz.SampleSize = nil
	stbl.Stsc.Entries = []mp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: perChunk, FirstSampleNr: 1}}
	stbl.Stsc.SetSingleSampleDescriptionID(1)
	stbl.Stco.ChunkOffset = make([]uint32, samples/perChunk)
}

func TestSubtitleIndexingStopsWhenContextIsCancelled(t *testing.T) {
	source := craftedSubtitleSource(t, func(stbl *mp4.StblBox) {
		floodChunks(stbl, 1_000_000, 10_000)
		stbl.Stts.SampleCount = []uint32{1_000_000}
		stbl.Stts.SampleTimeDelta = []uint32{1}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reads := &sizeRecorder{}
	_, err := extractBatchFrom(ctx, readerPair{ReaderAt: bytes.NewReader(source), recorder: reads}, int64(len(source)), []Track{{ID: "s", StreamIndex: 1, Codec: "mov_text"}})
	if err != context.Canceled {
		t.Fatalf("cancelled extraction returned %v, want context.Canceled", err)
	}
}

func TestSubtitleIndexDeclinesSamplesThatOutnumberTheSource(t *testing.T) {
	source := craftedSubtitleSource(t, func(stbl *mp4.StblBox) {
		floodChunks(stbl, 50_000_000, 10_000)
		stbl.Stts.SampleCount = []uint32{50_000_000}
		stbl.Stts.SampleTimeDelta = []uint32{1}
	})
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, failures, err := mp4index.SubtitleIndexesFrom(context.Background(), bytes.NewReader(source), int64(len(source)), []int{1})
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if failures[1] == nil {
		t.Errorf("a %d byte source claiming 50,000,000 subtitle samples was indexed", len(source))
	}
	if allocated := (after.TotalAlloc - before.TotalAlloc) >> 20; allocated > 32 {
		t.Errorf("indexing a %d byte source allocated %d MiB", len(source), allocated)
	}
}

type sizeRecorder struct {
	mu    sync.Mutex
	sizes []int
}

func (r *sizeRecorder) ReadAt(p []byte, _ int64) (int, error) {
	r.mu.Lock()
	r.sizes = append(r.sizes, len(p))
	r.mu.Unlock()
	return len(p), nil
}

type readerPair struct {
	*bytes.Reader
	ReaderAt *bytes.Reader
	recorder *sizeRecorder
}

func (r readerPair) ReadAt(p []byte, off int64) (int, error) {
	r.recorder.ReadAt(p, off)
	return r.ReaderAt.ReadAt(p, off)
}

func TestSubtitleReadsNeverSpanMoreThanTheCoalesceLimit(t *testing.T) {
	samples := make([]mp4index.SubtitleSample, 4000)
	for i := range samples {
		samples[i] = mp4index.SubtitleSample{Offset: int64(i) * 250_000, Size: 40, Duration: time.Second}
	}
	recorder := &sizeRecorder{}
	if _, err := readSamples(context.Background(), recorder, samples); err != nil {
		t.Fatal(err)
	}
	largest, total := 0, 0
	for _, size := range recorder.sizes {
		largest = max(largest, size)
		total += size
	}
	if largest > 4<<20 {
		t.Errorf("one read spans %d bytes for %d bytes of cues", largest, 4000*40)
	}
	if total > 32<<20 {
		t.Errorf("%d bytes requested for %d bytes of cues", total, 4000*40)
	}
}

func TestCorruptSubtitleSampleDropsOnlyItsCue(t *testing.T) {
	data, err := os.ReadFile(makeMovTextFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	indexes, _, err := mp4index.SubtitleIndexesFrom(context.Background(), bytes.NewReader(data), int64(len(data)), []int{1})
	if err != nil {
		t.Fatal(err)
	}
	corrupted := false
	for _, sample := range indexes[1].Samples {
		if sample.Size > 2 && bytes.Contains(data[sample.Offset:sample.Offset+int64(sample.Size)], []byte("Second line")) {
			data[sample.Offset] = 0x7f
			corrupted = true
		}
	}
	if !corrupted {
		t.Fatal("fixture cue not found")
	}
	outcomes, err := extractBatchFrom(context.Background(), bytes.NewReader(data), int64(len(data)), []Track{{ID: "s", StreamIndex: 1, Codec: "mov_text"}})
	if err != nil {
		t.Fatal(err)
	}
	outcome := outcomes["s"]
	if outcome.Err != nil {
		t.Fatalf("one corrupt sample failed the whole track: %v", outcome.Err)
	}
	text := string(outcome.Data)
	if !strings.Contains(text, "Hello &amp;") || strings.Contains(text, "Second line") {
		t.Fatalf("expected only the intact cue, got:\n%s", text)
	}
}

func TestTrackWithOnlyCorruptSamplesFails(t *testing.T) {
	data, err := os.ReadFile(makeMovTextFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	indexes, _, err := mp4index.SubtitleIndexesFrom(context.Background(), bytes.NewReader(data), int64(len(data)), []int{1})
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range indexes[1].Samples {
		if sample.Size > 0 {
			data[sample.Offset] = 0x7f
		}
	}
	outcomes, err := extractBatchFrom(context.Background(), bytes.NewReader(data), int64(len(data)), []Track{{ID: "s", StreamIndex: 1, Codec: "mov_text"}})
	if err != nil {
		t.Fatal(err)
	}
	if outcomes["s"].Err == nil {
		t.Fatalf("a track with no decodable sample produced %q", outcomes["s"].Data)
	}
}
