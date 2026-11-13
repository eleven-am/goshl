package subtitle

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/probe/mp4index"
	"github.com/eleven-am/goshl/internal/testmedia"
	"github.com/eleven-am/goshl/internal/webvtt"
)

type countingReaderAt struct {
	reader *os.File
	bytes  atomic.Int64
}

func (r *countingReaderAt) ReadAt(buffer []byte, offset int64) (int, error) {
	n, err := r.reader.ReadAt(buffer, offset)
	r.bytes.Add(int64(n))
	return n, err
}

func TestExtractBatchReadsOnlyIndexedSubtitleBytes(t *testing.T) {
	path := makeMovTextFixture(t)
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}
	counting := &countingReaderAt{reader: file}

	outcomes, err := extractBatchFrom(context.Background(), counting, info.Size(), []Track{{
		ID:          "english",
		StreamIndex: 1,
		Codec:       "mov_text",
	}})
	if err != nil {
		t.Fatalf("extract indexed subtitles: %v", err)
	}
	outcome := outcomes["english"]
	if !outcome.Supported || outcome.Err != nil {
		t.Fatalf("outcome = %+v, want supported result", outcome)
	}
	text := string(outcome.Data)
	for _, expected := range []string{
		"WEBVTT\nX-TIMESTAMP-MAP=",
		"00:00:00.500 --> 00:00:01.500",
		"Hello &amp;",
		"00:00:02.000 --> 00:00:03.250",
		"Second line\ncontinued",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("subtitle output is missing %q:\n%s", expected, text)
		}
	}
	if read := counting.bytes.Load(); read >= 1<<20 {
		t.Fatalf("read %d bytes from %d-byte source, want less than 1 MiB", read, info.Size())
	}
}

func TestExtractBatchMarksUnsupportedCodecsForFallback(t *testing.T) {
	path := makeMovTextFixture(t)
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}

	outcomes, err := extractBatchFrom(context.Background(), file, info.Size(), []Track{{
		ID:          "english",
		StreamIndex: 1,
		Codec:       "ass",
	}})
	if err != nil {
		t.Fatalf("extract indexed subtitles: %v", err)
	}
	if outcomes["english"].Supported {
		t.Fatal("unsupported codec was marked supported")
	}
}

func TestExtractBatchReadsSubtitleIndexAfterMediaData(t *testing.T) {
	path := makeMovTextFixtureAtEnd(t)
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}
	counting := &countingReaderAt{reader: file}

	outcomes, err := extractBatchFrom(context.Background(), counting, info.Size(), []Track{{
		ID:          "english",
		StreamIndex: 1,
		Codec:       "mov_text",
	}})
	if err != nil {
		t.Fatalf("extract indexed subtitles: %v", err)
	}
	if outcome := outcomes["english"]; !outcome.Supported || outcome.Err != nil || !strings.Contains(string(outcome.Data), "Second line") {
		t.Fatalf("outcome = %+v, want extracted subtitles", outcome)
	}
	if read := counting.bytes.Load(); read >= 1<<20 {
		t.Fatalf("read %d bytes from %d-byte source, want less than 1 MiB", read, info.Size())
	}
}

func TestExtractBatchUsesHTTPRangeRequests(t *testing.T) {
	path := makeMovTextFixture(t)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var transferred atomic.Int64
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		start, end, ok := testRange(request.Header.Get("Range"))
		if !ok || start < 0 || end >= int64(len(content)) {
			http.Error(writer, "invalid range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		requests.Add(1)
		transferred.Add(end - start + 1)
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = writer.Write(content[start : end+1])
	}))
	defer server.Close()

	outcomes, err := ExtractBatch(t.Context(), server.URL+"/captions.mp4", []Track{{
		ID:          "english",
		StreamIndex: 1,
		Codec:       "mov_text",
	}})
	if err != nil {
		t.Fatalf("extract HTTP subtitles: %v", err)
	}
	if outcome := outcomes["english"]; !outcome.Supported || outcome.Err != nil || !strings.Contains(string(outcome.Data), "Second line") {
		t.Fatalf("outcome = %+v, want extracted subtitles", outcome)
	}
	if transferred.Load() >= 1<<20 {
		t.Fatalf("transferred %d bytes from %d-byte source, want less than 1 MiB", transferred.Load(), len(content))
	}
	if requests.Load() < 3 {
		t.Fatalf("range requests = %d, want probe, index, and samples", requests.Load())
	}
}

func TestIndexedSubtitleCuesShareVideoTimeline(t *testing.T) {
	testmedia.Require(t)
	path := testmedia.H264(t, testmedia.Spec{Container: "mp4", Duration: 8, Subtitles: true, StartOffset: 1})
	origin := testmedia.FormatStart(t, path)
	if origin < 0.99 {
		t.Fatalf("fixture start_time = %v, want about 1 s", origin)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}
	originDuration := time.Duration(origin * float64(time.Second))

	outcomes, err := extractBatchFrom(context.Background(), file, info.Size(), []Track{
		{ID: "shifted", StreamIndex: 1, Codec: "mov_text", Origin: originDuration},
		{ID: "raw", StreamIndex: 1, Codec: "mov_text"},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	shifted, err := webvtt.Parse(outcomes["shifted"].Data)
	if err != nil || len(shifted) == 0 {
		t.Fatalf("parse shifted output: %v\n%s", err, outcomes["shifted"].Data)
	}
	raw, err := webvtt.Parse(outcomes["raw"].Data)
	if err != nil || len(raw) == 0 {
		t.Fatalf("parse raw output: %v\n%s", err, outcomes["raw"].Data)
	}
	if got := shifted[0].Start; got < 990*time.Millisecond || got > 1010*time.Millisecond {
		t.Errorf("first cue starts at %v on the video timeline, want 1 s (the script time):\n%s", got, outcomes["shifted"].Data)
	}
	if delta := raw[0].Start - shifted[0].Start; delta < originDuration-2*time.Millisecond || delta > originDuration+2*time.Millisecond {
		t.Errorf("raw minus shifted = %v, want the origin %v", delta, originDuration)
	}
}

func TestIndexedSubtitleVTTCarriesTimestampMap(t *testing.T) {
	path := makeMovTextFixture(t)
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}
	outcomes, err := extractBatchFrom(context.Background(), file, info.Size(), []Track{{ID: "english", StreamIndex: 1, Codec: "mov_text"}})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	wantMap := fmt.Sprintf("X-TIMESTAMP-MAP=MPEGTS:%d,LOCAL:00:00:00.000", int64(domain.OutputTimelineOffset.Seconds()*90000))
	if !strings.HasPrefix(string(outcomes["english"].Data), "WEBVTT\n"+wantMap+"\n\n") {
		t.Fatalf("indexed vtt lacks %q:\n%s", wantMap, outcomes["english"].Data)
	}
}

func TestDecodeTX3GEscapesOnlyVTTSpecialCharacters(t *testing.T) {
	text := "It's \"fine\" & <ok>"
	data := append([]byte{0, byte(len(text))}, text...)
	got, err := decodeTX3G(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if want := `It's "fine" &amp; &lt;ok&gt;`; got != want {
		t.Fatalf("decoded text = %q, want %q", got, want)
	}
}

func TestDecodeTX3GRejectsTruncatedText(t *testing.T) {
	_, err := decodeTX3G([]byte{0, 5, 'h', 'i'})
	if err == nil {
		t.Fatal("decode truncated tx3g sample succeeded")
	}
}

func TestDecodeTX3GAcceptsEmptySample(t *testing.T) {
	text, err := decodeTX3G(nil)
	if err != nil || text != "" {
		t.Fatalf("decode empty tx3g sample = %q, %v", text, err)
	}
}

func TestDecodeTX3GSupportsUTF16(t *testing.T) {
	text := []uint16{0xfeff, 'H', 'i', ' ', 0x263a}
	data := make([]byte, 2+len(text)*2)
	binary.BigEndian.PutUint16(data[:2], uint16(len(text)*2))
	for index, value := range text {
		binary.BigEndian.PutUint16(data[2+index*2:], value)
	}
	got, err := decodeTX3G(data)
	if err != nil {
		t.Fatalf("decode UTF-16 tx3g: %v", err)
	}
	if got != "Hi ☺" {
		t.Fatalf("decoded text = %q, want %q", got, "Hi ☺")
	}
}

func TestDecodeWVTTKeepsCueMetadata(t *testing.T) {
	cue := &mp4.VttcBox{}
	cue.AddChild(&mp4.IdenBox{CueID: "speaker-1"})
	cue.AddChild(&mp4.SttgBox{Settings: "align:start position:10%"})
	cue.AddChild(&mp4.PaylBox{CueText: "Hello from WebVTT"})
	var encoded bytes.Buffer
	if err := cue.Encode(&encoded); err != nil {
		t.Fatalf("encode wvtt cue: %v", err)
	}

	cues, err := decodeWVTT(encoded.Bytes())
	if err != nil {
		t.Fatalf("decode wvtt cue: %v", err)
	}
	if len(cues) != 1 {
		t.Fatalf("cue count = %d, want 1", len(cues))
	}
	if cues[0].id != "speaker-1" || cues[0].settings != "align:start position:10%" || cues[0].text != "Hello from WebVTT" {
		t.Fatalf("decoded cue = %+v", cues[0])
	}
}

func makeMovTextFixture(t *testing.T) string {
	return makeMovTextFixtureWithLayout(t, true)
}

func makeMovTextFixtureAtEnd(t *testing.T) string {
	return makeMovTextFixtureWithLayout(t, false)
}

func makeMovTextFixtureWithLayout(t *testing.T, fastStart bool) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is unavailable")
	}
	dir := t.TempDir()
	subtitlePath := filepath.Join(dir, "captions.srt")
	mediaPath := filepath.Join(dir, "captions.mp4")
	srt := "1\n00:00:00,500 --> 00:00:01,500\nHello & <world>\n\n2\n00:00:02,000 --> 00:00:03,250\nSecond line\ncontinued\n"
	if err := os.WriteFile(subtitlePath, []byte(srt), 0o600); err != nil {
		t.Fatalf("write subtitle fixture: %v", err)
	}
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "color=c=black:s=320x180:r=25:d=4",
		"-i", subtitlePath,
		"-map", "0:v", "-map", "1:s",
		"-c:v", "libx264", "-c:s", "mov_text",
	}
	if fastStart {
		args = append(args, "-movflags", "+faststart")
	}
	args = append(args, "-t", "4", mediaPath)
	command := exec.Command("ffmpeg", args...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build subtitle fixture: %v: %s", err, output)
	}
	file, err := os.OpenFile(mediaPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("open fixture for padding: %v", err)
	}
	if err := file.Truncate(32 << 20); err != nil {
		file.Close()
		t.Fatalf("pad fixture: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}
	return mediaPath
}

func testRange(value string) (int64, int64, bool) {
	if !strings.HasPrefix(value, "bytes=") {
		return 0, 0, false
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes="), "-")
	if len(parts) != 2 {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	end, err := strconv.ParseInt(parts[1], 10, 64)
	return start, end, err == nil && start <= end
}

var _ io.ReaderAt = (*countingReaderAt)(nil)

type rangeLoggingReaderAt struct {
	data  []byte
	reads [][2]int64
	mu    sync.Mutex
}

func (r *rangeLoggingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	r.reads = append(r.reads, [2]int64{off, int64(len(p))})
	r.mu.Unlock()
	return copy(p, r.data[off:]), nil
}

func TestIndexedSubtitleReadCoalescesRanges(t *testing.T) {
	data := make([]byte, 2<<20)
	for index := range data {
		data[index] = byte(index % 251)
	}
	samples := []mp4index.SubtitleSample{
		{Offset: 1000, Size: 10},
		{Offset: 1000 + 10 + 4096, Size: 20},
		{Offset: 1000 + 10 + 4096 + 20 + maxCoalesceGap, Size: 5},
		{Offset: 1500000, Size: 7},
	}
	source := &rangeLoggingReaderAt{data: data}
	got, err := readSamples(t.Context(), source, samples)
	if err != nil {
		t.Fatalf("read samples: %v", err)
	}
	for index, sample := range samples {
		want := data[sample.Offset : sample.Offset+int64(sample.Size)]
		if !bytes.Equal(got[index], want) {
			t.Fatalf("sample %d = %v, want %v", index, got[index], want)
		}
	}
	if len(source.reads) != 2 {
		t.Fatalf("reads = %v, want 2 coalesced reads", source.reads)
	}
}
