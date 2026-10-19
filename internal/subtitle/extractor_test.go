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
	"sync/atomic"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
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
		"WEBVTT\n\n",
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
