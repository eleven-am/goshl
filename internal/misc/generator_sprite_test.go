package misc

import (
	"bytes"
	"context"
	"fmt"
	"image/jpeg"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
)

func TestSpriteArgsDecodeOnlyKeyframesBeforeInput(t *testing.T) {
	g := NewGenerator(&stubStorage{}, &domain.HWAccelConfig{
		Accelerator: domain.AccelCUDA,
		DecodeFlags: []string{"-hwaccel", "cuda"},
	})

	args := g.spriteArgs("input.mp4", "sprite-%d.jpg", 100)

	skip := indexOf(args, "-skip_frame")
	input := indexOf(args, "-i")
	if skip == -1 || skip+1 >= len(args) || args[skip+1] != "nokey" {
		t.Fatalf("expected -skip_frame nokey, got %v", args)
	}
	if input == -1 || skip > input {
		t.Fatalf("expected -skip_frame before -i, got %v", args)
	}
	if hw := indexOf(args, "-hwaccel"); hw == -1 || hw > skip {
		t.Fatalf("expected decode flags before -skip_frame, got %v", args)
	}
}

func TestSpriteFilterFillsKeyframeGapsAndBoundsThumbCount(t *testing.T) {
	g := NewGenerator(&stubStorage{})

	filter := g.spriteFilter(97)

	for _, want := range []string{"tpad=stop_mode=clone:stop_duration=102", "fps=1/5", "trim=end_frame=20", "tile=10x10"} {
		if !strings.Contains(filter, want) {
			t.Fatalf("expected %q in %q", want, filter)
		}
	}
	if strings.Index(filter, "tpad") > strings.Index(filter, "fps") || strings.Index(filter, "fps") > strings.Index(filter, "trim") || strings.Index(filter, "trim") > strings.Index(filter, "tile") {
		t.Fatalf("unexpected filter order %q", filter)
	}
}

func TestSpriteFilterPadsAfterHardwareDownload(t *testing.T) {
	g := NewGenerator(&stubStorage{}, &domain.HWAccelConfig{
		Accelerator:  domain.AccelCUDA,
		ScaleFilter:  "scale_cuda=%d:%d:format=nv12",
		DeviceFrames: true,
	})

	filter := g.spriteFilter(60)

	if strings.Index(filter, "hwdownload") > strings.Index(filter, "tpad") || !strings.Contains(filter, "trim=end_frame=12") {
		t.Fatalf("unexpected filter %q", filter)
	}
}

func TestSetSpriteStorageIgnoresNil(t *testing.T) {
	storage := &stubStorage{}
	g := NewGenerator(storage)
	g.SetSpriteStorage(nil)
	if g.spriteBackend() != domain.SpriteStorage(storage) {
		t.Fatalf("nil sprite storage must keep the default")
	}
}

func TestSpritesUseSeparateSpriteStorage(t *testing.T) {
	main := &stubStorage{}
	sprites := &stubStorage{spriteVTTExists: true, spriteVTTData: []byte("from-sprites"), spriteData: map[int][]byte{0: []byte("img")}}
	g := NewGenerator(main)
	g.SetSpriteStorage(sprites)

	vtt, err := g.GetSpriteVTT(context.Background(), "file:///media", "key", 10, spriteURL)
	if err != nil || string(vtt) != "from-sprites" {
		t.Fatalf("expected vtt from sprite storage, got %q %v", vtt, err)
	}
	img, err := g.GetSprite(context.Background(), "file:///media", "key", 10, spriteURL, 0)
	if err != nil || string(img) != "img" {
		t.Fatalf("expected sprite from sprite storage, got %q %v", img, err)
	}
	if main.readSprites != 0 || main.wroteSprites != 0 || main.wroteSpriteVTT != 0 {
		t.Fatalf("segment storage must not serve sprites")
	}
}

func TestGenerateSpritesFromKeyframesProducesValidVTT(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	clip := filepath.Join(t.TempDir(), "clip.mp4")
	build := exec.Command("ffmpeg", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=320x180:rate=10:duration=97",
		"-c:v", "libx264", "-g", "200", "-keyint_min", "200", "-sc_threshold", "0", "-pix_fmt", "yuv420p", clip)
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("cannot build clip: %v %s", err, out)
	}

	main := &stubStorage{}
	sprites := &stubStorage{}
	g := NewGenerator(main)
	g.SetSpriteStorage(sprites)

	vtt, err := g.GetSpriteVTT(context.Background(), clip, "key", 97, spriteURL)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	text := string(vtt)
	if !strings.HasPrefix(text, "WEBVTT") || strings.Count(text, "-->") != 20 {
		t.Fatalf("expected 20 cues, got %s", text)
	}
	if !strings.Contains(text, "00:01:35.000 --> 00:01:37.000") {
		t.Fatalf("expected final cue to end at duration, got %s", text)
	}
	if sprites.wroteSprites != 1 || sprites.wroteSpriteVTT != 1 {
		t.Fatalf("expected one sprite sheet and one vtt, got %d %d", sprites.wroteSprites, sprites.wroteSpriteVTT)
	}
	if main.wroteSprites != 0 || main.wroteSpriteVTT != 0 {
		t.Fatalf("segment storage must not receive sprites")
	}
	img, err := jpeg.Decode(bytes.NewReader(sprites.spriteData[0]))
	if err != nil {
		t.Fatalf("decode sprite: %v", err)
	}
	if img.Bounds().Dx() != 1600 || img.Bounds().Dy() != 900 {
		t.Fatalf("unexpected sprite size %v", img.Bounds())
	}
}

func TestGenerateSpritesFillsThumbsAfterLastKeyframe(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	clip := filepath.Join(t.TempDir(), "clip.mp4")
	build := exec.Command("ffmpeg", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=320x180:rate=10:duration=97",
		"-c:v", "libx264", "-g", "200", "-keyint_min", "200", "-sc_threshold", "0", "-pix_fmt", "yuv420p", clip)
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("cannot build clip: %v %s", err, out)
	}

	tmp := t.TempDir()
	args := NewGenerator(&stubStorage{}).spriteArgs(clip, filepath.Join(tmp, "s-%d.jpg"), 97)
	args = append([]string{"-y"}, args...)
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	matches, _ := filepath.Glob(filepath.Join(tmp, "s-*.jpg"))
	if len(matches) != 1 {
		t.Fatalf("expected exactly one sprite sheet, got %v", matches)
	}
}

func buildSingleKeyframeClip(t *testing.T, seconds int) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	clip := filepath.Join(t.TempDir(), "clip.mp4")
	build := exec.Command("ffmpeg", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=320x180:rate=10:duration=%d", seconds),
		"-c:v", "libx264", "-g", "1000", "-keyint_min", "1000", "-sc_threshold", "0", "-pix_fmt", "yuv420p", clip)
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("cannot build clip: %v %s", err, out)
	}
	return clip
}

func TestGenerateSpritesForShortSingleKeyframeClips(t *testing.T) {
	for _, tc := range []struct {
		seconds int
		cues    int
	}{{2, 1}, {6, 2}, {11, 3}, {12, 3}} {
		t.Run(fmt.Sprintf("%ds", tc.seconds), func(t *testing.T) {
			clip := buildSingleKeyframeClip(t, tc.seconds)
			sprites := &stubStorage{}
			g := NewGenerator(&stubStorage{})
			g.SetSpriteStorage(sprites)

			vtt, err := g.GetSpriteVTT(context.Background(), clip, "key", float64(tc.seconds), spriteURL)
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			if !strings.HasPrefix(string(vtt), "WEBVTT") || strings.Count(string(vtt), "-->") != tc.cues {
				t.Fatalf("expected %d cues, got %s", tc.cues, vtt)
			}
			if sprites.wroteSprites != 1 {
				t.Fatalf("expected one sheet, got %d", sprites.wroteSprites)
			}
			if _, err := jpeg.Decode(bytes.NewReader(sprites.spriteData[0])); err != nil {
				t.Fatalf("decode sprite: %v", err)
			}
		})
	}
}
