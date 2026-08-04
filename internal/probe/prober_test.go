package probe

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
)

type stubStorage struct {
	exists    bool
	metaData  []byte
	existsCnt int
	getCnt    int
	setCnt    int
}

func (s *stubStorage) MetadataExists(ctx context.Context, sourceURL string) (bool, error) {
	s.existsCnt++
	return s.exists, nil
}

func (s *stubStorage) GetMetadata(ctx context.Context, sourceURL string) ([]byte, error) {
	s.getCnt++
	return s.metaData, nil
}

func (s *stubStorage) SetMetadata(ctx context.Context, sourceURL string, data []byte) error {
	s.setCnt++
	s.metaData = data
	s.exists = true
	return nil
}

func (s *stubStorage) WriteSegment(ctx context.Context, info domain.SegmentData, data []byte) error {
	return nil
}
func (s *stubStorage) ReadSegment(ctx context.Context, info domain.SegmentData) ([]byte, error) {
	return nil, nil
}
func (s *stubStorage) SegmentExists(ctx context.Context, info domain.SegmentData) (bool, error) {
	return false, nil
}
func (s *stubStorage) WriteSprite(ctx context.Context, sourceURL string, index int, data []byte) error {
	return nil
}
func (s *stubStorage) ReadSprite(ctx context.Context, sourceURL string, index int) ([]byte, error) {
	return nil, nil
}
func (s *stubStorage) SpriteExists(ctx context.Context, sourceURL string, index int) (bool, error) {
	return false, nil
}
func (s *stubStorage) WriteSpriteVTT(ctx context.Context, sourceURL string, data []byte) error {
	return nil
}
func (s *stubStorage) ReadSpriteVTT(ctx context.Context, sourceURL string) ([]byte, error) {
	return nil, nil
}
func (s *stubStorage) SpriteVTTExists(ctx context.Context, sourceURL string) (bool, error) {
	return false, nil
}
func (s *stubStorage) WriteSubtitleVTT(ctx context.Context, sourceURL string, lang string, data []byte) error {
	return nil
}
func (s *stubStorage) ReadSubtitleVTT(ctx context.Context, sourceURL string, lang string) ([]byte, error) {
	return nil, nil
}
func (s *stubStorage) SubtitleVTTExists(ctx context.Context, sourceURL string, lang string) (bool, error) {
	return false, nil
}
func (s *stubStorage) WritePreview(ctx context.Context, sourceURL string, data []byte) error {
	return nil
}
func (s *stubStorage) ReadPreview(ctx context.Context, sourceURL string) ([]byte, error) {
	return nil, nil
}
func (s *stubStorage) PreviewExists(ctx context.Context, sourceURL string) (bool, error) {
	return false, nil
}
func (s *stubStorage) WriteThumbnail(ctx context.Context, sourceURL string, data []byte) error {
	return nil
}
func (s *stubStorage) ReadThumbnail(ctx context.Context, sourceURL string) ([]byte, error) {
	return nil, nil
}
func (s *stubStorage) ThumbnailExists(ctx context.Context, sourceURL string) (bool, error) {
	return false, nil
}

func TestProbe_UsesCacheAndSkipsFFProbe(t *testing.T) {
	cached := &domain.Metadata{Duration: 5}
	cachedBytes, _ := json.Marshal(cached)
	storage := &stubStorage{exists: true, metaData: cachedBytes}
	p := NewProber(storage)
	got, err := p.Probe(context.Background(), "file:///cached")
	if err != nil {
		t.Fatalf("probe returned error: %v", err)
	}

	if got.Duration != cached.Duration {
		t.Fatalf("expected cached metadata duration %v, got %v", cached.Duration, got.Duration)
	}
	if storage.setCnt != 0 {
		t.Fatalf("cache hit should not attempt to set metadata")
	}
}

func TestProbe_InvokesFFProbeAndPersistsMetadata(t *testing.T) {
	tmpDir := t.TempDir()
	script := filepath.Join(tmpDir, "ffprobe")
	if err := os.WriteFile(script, []byte(ffprobeScript), 0755); err != nil {
		t.Fatalf("failed to write fake ffprobe: %v", err)
	}

	originalPath := os.Getenv("PATH")
	t.Cleanup(func() { _ = os.Setenv("PATH", originalPath) })
	if err := os.Setenv("PATH", tmpDir+string(os.PathListSeparator)+originalPath); err != nil {
		t.Fatalf("failed to update PATH: %v", err)
	}

	storage := &stubStorage{}
	p := NewProber(storage)

	meta, err := p.Probe(context.Background(), "file:///input")
	if err != nil {
		t.Fatalf("probe returned error: %v", err)
	}

	if meta.Duration != 12.5 {
		t.Fatalf("unexpected duration: %v", meta.Duration)
	}
	if meta.Video.Codec != "h264" || meta.Video.Width != 1920 || meta.Video.Height != 1080 {
		t.Fatalf("unexpected video stream: %#v", meta.Video)
	}
	if meta.Video.Bitrate != 6_000_000 {
		t.Fatalf("expected video bitrate from BPS tag, got %d", meta.Video.Bitrate)
	}
	if meta.Video.FrameRate < 29.9 || meta.Video.FrameRate > 30.1 {
		t.Fatalf("expected parsed framerate around 29.97, got %f", meta.Video.FrameRate)
	}

	if len(meta.Audios) != 1 {
		t.Fatalf("expected one audio stream, got %d", len(meta.Audios))
	}
	if a := meta.Audios[0]; a.Codec != "ac3" || a.Channels != 6 || a.Bitrate != 640000 || a.Language != "eng" {
		t.Fatalf("unexpected audio: %#v", a)
	}

	if got := len(meta.Keyframes); got != 3 {
		t.Fatalf("expected 3 keyframes parsed, got %d (%#v)", got, meta.Keyframes)
	}
	if meta.Keyframes[0] != 0 || meta.Keyframes[1] != 3 || meta.Keyframes[2] != 6.2 {
		t.Fatalf("unexpected keyframes parsed: %#v", meta.Keyframes)
	}

	if storage.setCnt != 1 {
		t.Fatalf("metadata should be persisted once, got %d", storage.setCnt)
	}
}

func TestProbe_UsesStreamBitRateBeforeBPSTag(t *testing.T) {
	tmpDir := t.TempDir()
	script := filepath.Join(tmpDir, "ffprobe")
	if err := os.WriteFile(script, []byte(ffprobeBitRateScript), 0755); err != nil {
		t.Fatalf("failed to write fake ffprobe: %v", err)
	}

	originalPath := os.Getenv("PATH")
	t.Cleanup(func() { _ = os.Setenv("PATH", originalPath) })
	if err := os.Setenv("PATH", tmpDir+string(os.PathListSeparator)+originalPath); err != nil {
		t.Fatalf("failed to update PATH: %v", err)
	}

	meta, err := NewProber(&stubStorage{}).Probe(context.Background(), "file:///input")
	if err != nil {
		t.Fatalf("probe returned error: %v", err)
	}

	if meta.Video.Bitrate != 4_000_000 {
		t.Fatalf("expected video bitrate from bit_rate, got %d", meta.Video.Bitrate)
	}
}

func TestProbe_NormalizesNonZeroStartTimeAndSelectsPlayableVideo(t *testing.T) {
	tmpDir := t.TempDir()
	script := filepath.Join(tmpDir, "ffprobe")
	if err := os.WriteFile(script, []byte(ffprobeNonZeroStartScript), 0755); err != nil {
		t.Fatalf("failed to write fake ffprobe: %v", err)
	}

	originalPath := os.Getenv("PATH")
	t.Cleanup(func() { _ = os.Setenv("PATH", originalPath) })
	if err := os.Setenv("PATH", tmpDir+string(os.PathListSeparator)+originalPath); err != nil {
		t.Fatalf("failed to update PATH: %v", err)
	}

	meta, err := NewProber(&stubStorage{}).Probe(context.Background(), "file:///input")
	if err != nil {
		t.Fatalf("probe returned error: %v", err)
	}

	if meta.StartTime != 1.466667 {
		t.Fatalf("expected original start time, got %f", meta.StartTime)
	}
	if meta.Video.Index != 1 {
		t.Fatalf("expected playable video stream index 1, got %d", meta.Video.Index)
	}
	if meta.Video.PixelFormat != "yuv420p" || meta.Video.Profile != "High" || meta.Video.Level != 40 {
		t.Fatalf("missing compatibility metadata: %#v", meta.Video)
	}
	if meta.Video.Width != 1080 || meta.Video.Height != 1920 || meta.Video.Rotation != 90 {
		t.Fatalf("expected display-oriented dimensions: %#v", meta.Video)
	}
	want := []float64{0, 2, 4}
	if len(meta.Keyframes) != len(want) {
		t.Fatalf("expected %d normalized keyframes, got %#v", len(want), meta.Keyframes)
	}
	for i := range want {
		if meta.Keyframes[i] < want[i]-0.000001 || meta.Keyframes[i] > want[i]+0.000001 {
			t.Fatalf("keyframe %d: expected %f, got %f", i, want[i], meta.Keyframes[i])
		}
	}
}

func TestProbe_NormalizesRealMPEGTSFixture(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is unavailable")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe is unavailable")
	}

	output := filepath.Join(t.TempDir(), "nonzero.ts")
	cmd := exec.Command(ffmpegPath,
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=25",
		"-t", "4", "-c:v", "libx264", "-g", "25", "-keyint_min", "25", "-sc_threshold", "0",
		"-f", "mpegts", output,
	)
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create fixture: %v: %s", err, data)
	}

	meta, err := NewProber(&stubStorage{}).Probe(context.Background(), output)
	if err != nil {
		t.Fatalf("probe fixture: %v", err)
	}
	if meta.StartTime <= 0 {
		t.Fatalf("fixture did not have a non-zero origin: %f", meta.StartTime)
	}
	if len(meta.Keyframes) == 0 || meta.Keyframes[0] < -0.001 || meta.Keyframes[0] > 0.001 {
		t.Fatalf("keyframes were not normalized: %#v", meta.Keyframes)
	}
	for _, keyframe := range meta.Keyframes {
		if keyframe < 0 || keyframe > meta.Duration {
			t.Fatalf("keyframe outside playback timeline: %f of %f", keyframe, meta.Duration)
		}
	}
}

func TestValidateMetadataRejectsUnplayableInputs(t *testing.T) {
	for _, metadata := range []*domain.Metadata{
		nil,
		{Duration: 0, Audios: []domain.AudioStream{{Codec: "aac"}}},
		{Duration: 10},
		{Duration: 10, Video: domain.VideoStream{Codec: "h264"}},
	} {
		if err := validateMetadata(metadata); err == nil {
			t.Fatalf("expected invalid metadata rejection: %#v", metadata)
		}
	}
	if err := validateMetadata(&domain.Metadata{Duration: 10, Audios: []domain.AudioStream{{Codec: "aac"}}}); err != nil {
		t.Fatalf("audio-only media must remain playable: %v", err)
	}
}

func TestDisplayDimensionsApplySampleAspectBeforeRotation(t *testing.T) {
	width, height := displayDimensions(720, 576, "16:15", 0)
	if width != 768 || height != 576 {
		t.Fatalf("unexpected anamorphic dimensions %dx%d", width, height)
	}
	width, height = displayDimensions(720, 576, "16:15", -90)
	if width != 576 || height != 768 {
		t.Fatalf("unexpected rotated dimensions %dx%d", width, height)
	}
}

const ffprobeScript = `#!/bin/sh
if printf "%s" "$*" | grep -q "show_entries"; then
  cat <<'EOF'
0.000000,K
1.500000,.
3.000000,K
6.200000,K
EOF
  exit 0
fi

if printf "%s" "$*" | grep -q "show_format"; then
  cat <<'EOF'
{"streams":[{"index":0,"codec_name":"h264","codec_type":"video","width":1920,"height":1080,"r_frame_rate":"30000/1001","tags":{"BPS":"6000000"}},{"index":1,"codec_name":"ac3","codec_type":"audio","channels":6,"bit_rate":"640000","tags":{"language":"eng"}}],"format":{"duration":"12.5"}}
EOF
  exit 0
fi

echo "unexpected args: $*" >&2
exit 1
`

const ffprobeBitRateScript = `#!/bin/sh
if printf "%s" "$*" | grep -q "show_entries"; then
  exit 0
fi

if printf "%s" "$*" | grep -q "show_format"; then
  cat <<'EOF'
{"streams":[{"index":0,"codec_name":"h264","codec_type":"video","width":1920,"height":1080,"r_frame_rate":"30/1","bit_rate":"4000000","tags":{"BPS":"6000000"}}],"format":{"duration":"12.5"}}
EOF
  exit 0
fi

exit 1
`

const ffprobeNonZeroStartScript = `#!/bin/sh
if printf "%s" "$*" | grep -q "show_entries"; then
  cat <<'EOF'
1.466667,K
3.466667,K
5.466667,K
EOF
  exit 0
fi

if printf "%s" "$*" | grep -q "show_format"; then
  cat <<'EOF'
{"streams":[{"index":0,"codec_name":"mjpeg","codec_type":"video","width":600,"height":600,"disposition":{"attached_pic":1}},{"index":1,"codec_name":"h264","codec_type":"video","width":1920,"height":1080,"profile":"High","level":40,"pix_fmt":"yuv420p","r_frame_rate":"24/1","disposition":{"default":1},"side_data_list":[{"rotation":90}]}],"format":{"start_time":"1.466667","duration":"10.0"}}
EOF
  exit 0
fi

exit 1
`
