package ffmpeg

import (
	"context"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/playlist"
	"github.com/eleven-am/goshl/internal/testmedia"
)

const (
	integrationTick  = 1.0 / 90000
	integrationFrame = 1001.0 / 24000
)

type integrationFixture struct {
	path      string
	keyframes []float64
	segments  []domain.Segment
}

func newIntegrationFixture(t *testing.T, container string) integrationFixture {
	t.Helper()
	path := testmedia.H264(t, testmedia.Spec{
		Container:      container,
		BFrames:        3,
		Pyramid:        true,
		KeyframeFrames: []int{0, 50, 101, 149, 200, 251},
		Duration:       12,
		Audio:          []testmedia.AudioSpec{{Codec: "aac"}, {Codec: "ac3"}},
	})
	fixture := integrationFixture{path: path}
	start := testmedia.FormatStart(t, path)
	for _, packet := range testmedia.Packets(t, path, "v:0") {
		if packet.Key {
			fixture.keyframes = append(fixture.keyframes, packet.PTS-start)
		}
	}
	sort.Float64s(fixture.keyframes)
	fixture.segments = playlist.CalculateSegments(fixture.keyframes, 12, 2)
	if len(fixture.segments) < 5 {
		t.Fatalf("fixture must plan at least 5 segments, planned %d", len(fixture.segments))
	}
	return fixture
}

func integrationBuilder(encode ...string) *CommandBuilder {
	return NewCommandBuilder(&domain.HWAccelConfig{
		Accelerator:  domain.AccelNone,
		EncodeFlags:  append([]string{"-c:v", "libx264"}, encode...),
		KeyframeFlag: "-force_key_frames",
		ScaleFilter:  "scale=%d:%d,setsar=1,format=yuv420p",
	})
}

func runJob(t *testing.T, args []string, err error) []string {
	t.Helper()
	if err != nil {
		t.Fatalf("build job: %v", err)
	}
	cmd := exec.Command("ffmpeg", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ffmpeg failed: %v: %s", err, stderr.String())
	}
	return strings.Fields(string(out))
}

func measureLanding(t *testing.T, builder *CommandBuilder, path string, start float64, stream int) *float64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	value, err := builder.SeekLanding(ctx, path, start, stream)
	if err != nil {
		t.Fatalf("measure landing: %v", err)
	}
	return &value
}

func segmentPTS(t *testing.T, dir string, index int, selector string) ([]float64, []float64) {
	t.Helper()
	var all, keys []float64
	for _, packet := range testmedia.Packets(t, filepath.Join(dir, fmt.Sprintf("segment-%05d.ts", index)), selector) {
		all = append(all, packet.PTS)
		if packet.Key {
			keys = append(keys, packet.PTS)
		}
	}
	if len(all) == 0 {
		t.Fatalf("segment %d has no %s packets", index, selector)
	}
	sort.Float64s(all)
	return all, keys
}

func TestSoftwareTranscodeCoversNonZeroTimestampSource(t *testing.T) {
	testNonZeroTimestampCoverage(t, domain.Transcode)
}

func TestDirectStreamCoversNonZeroTimestampSource(t *testing.T) {
	testNonZeroTimestampCoverage(t, domain.DirectStream)
}

func testNonZeroTimestampCoverage(t *testing.T, method domain.PlaybackMethod) {
	t.Helper()
	input := testmedia.H264(t, testmedia.Spec{Container: "ts", FPS: "25", KeyframeFrames: []int{0, 25, 50, 75}, Duration: 4})
	if start := testmedia.FormatStart(t, input); start <= 0 {
		t.Fatalf("fixture must start at a non-zero timestamp, starts at %.3f", start)
	}
	builder := integrationBuilder()
	outputDir := t.TempDir()
	params := VideoParams{
		InputURL:    input,
		StreamIndex: 0,
		Rendition:   domain.VideoRendition{Method: method, Width: 320, Height: 180, Bitrate: 500_000},
		Segments: []domain.Segment{
			{Index: 0, Start: 0, End: 2, Duration: 2},
			{Index: 1, Start: 2, End: 4, Duration: 2},
		},
		OutputDir: outputDir,
	}
	if method == domain.DirectStream {
		params.Landing = measureLanding(t, builder, input, 0, 0)
	}
	args, err := builder.Video(params)
	if listed := runJob(t, args, err); len(listed) != 2 {
		t.Fatalf("%s job listed %v, want two segments", method, listed)
	}

	offset := domain.OutputTimelineOffset.Seconds()
	frame := 1.0 / 25
	for index, start := range []float64{0, 2} {
		pts, _ := segmentPTS(t, outputDir, index, "v:0")
		if diff := pts[0] - (start + offset); math.Abs(diff) > frame/2 {
			t.Fatalf("%s segment %d starts at %.6f, want %.6f", method, index, pts[0], start+offset)
		}
		if covered := pts[len(pts)-1] + frame - pts[0]; math.Abs(covered-2) > frame {
			t.Fatalf("%s segment %d covers %.3fs instead of 2s", method, index, covered)
		}
	}
	if method == domain.Transcode {
		data, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=profile,pix_fmt", "-of", "csv=p=0", filepath.Join(outputDir, "segment-00000.ts")).Output()
		if err != nil {
			t.Fatalf("probe transcoded video compatibility: %v", err)
		}
		compatibility := strings.ToLower(strings.TrimSpace(string(data)))
		if !strings.Contains(compatibility, "high") || !strings.Contains(compatibility, "yuv420p") {
			t.Fatalf("unexpected browser compatibility profile: %s", compatibility)
		}
	}
}

func TestSingleSegmentJobProducesExactlyOneFile(t *testing.T) {
	fixture := newIntegrationFixture(t, "mp4")
	t.Run("aac", func(t *testing.T) {
		args, err := integrationBuilder().Audio(AudioParams{InputURL: fixture.path, StreamIndex: 1, Rendition: domain.AudioRendition{Method: domain.Transcode, Channels: 2, Bitrate: 128000}, Segments: []domain.Segment{{Index: 0, Start: 0, End: 5, Duration: 5}}, OutputDir: t.TempDir()})
		if listed := runJob(t, args, err); len(listed) != 1 {
			t.Fatalf("one planned segment produced %d files: %v", len(listed), listed)
		}
	})
	t.Run("transcode", func(t *testing.T) {
		args, err := integrationBuilder("-g", "60").Video(VideoParams{InputURL: fixture.path, StreamIndex: 0, Rendition: domain.VideoRendition{Method: domain.Transcode, Width: 320, Height: 180, Bitrate: 400000}, Segments: []domain.Segment{{Index: 0, Start: 0, End: 6, Duration: 6}}, OutputDir: t.TempDir()})
		if listed := runJob(t, args, err); len(listed) != 1 {
			t.Fatalf("one planned segment produced %d files: %v", len(listed), listed)
		}
	})
	t.Run("copy", func(t *testing.T) {
		builder := integrationBuilder()
		end := fixture.segments[2].End
		args, err := builder.Video(VideoParams{InputURL: fixture.path, StreamIndex: 0, Rendition: domain.VideoRendition{Method: domain.DirectStream}, Segments: []domain.Segment{{Index: 0, Start: 0, End: end, Duration: end}}, Landing: measureLanding(t, builder, fixture.path, 0, 0), OutputDir: t.TempDir()})
		if listed := runJob(t, args, err); len(listed) != 1 {
			t.Fatalf("one planned segment produced %d files: %v", len(listed), listed)
		}
	})
}

func TestSeekLandingStopsAfterOnePacketOnAudio(t *testing.T) {
	fixture := newIntegrationFixture(t, "mkv")
	started := time.Now()
	landingArgs, err := seekLandingArgs(fixture.path, 5, 1)
	if err != nil {
		t.Fatalf("seek landing args: %v", err)
	}
	out, err := exec.Command("ffmpeg", landingArgs...).Output()
	if err != nil {
		t.Fatalf("seek landing: %v", err)
	}
	packets := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "#") {
			packets++
		}
	}
	if packets != 1 {
		t.Fatalf("seek landing read %d packets, want 1", packets)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("seek landing took %v", elapsed)
	}
}

func TestCopyTrailingSegmentAbsorbsDTSLeak(t *testing.T) {
	fixture := newIntegrationFixture(t, "mp4")
	builder := integrationBuilder()
	dir := t.TempDir()
	args, err := builder.Video(VideoParams{
		InputURL:    fixture.path,
		StreamIndex: 0,
		Rendition:   domain.VideoRendition{Method: domain.DirectStream},
		Segments:    fixture.segments[0:2],
		Trailing:    &fixture.segments[2],
		Landing:     measureLanding(t, builder, fixture.path, 0, 0),
		OutputDir:   dir,
	})
	if listed := runJob(t, args, err); len(listed) != 3 {
		t.Fatalf("job with a trailing segment listed %v, want segments 0, 1 and the trailing 2", listed)
	}
	next := fixture.segments[2].Start + domain.OutputTimelineOffset.Seconds()
	pts, _ := segmentPTS(t, dir, 1, "v:0")
	if last := pts[len(pts)-1]; last >= next-integrationTick {
		t.Fatalf("segment 1 reaches PTS %.6f, the next job starts at keyframe %.6f", last, next)
	}
	if gap := next - pts[len(pts)-1]; math.Abs(gap-integrationFrame) > 2*integrationTick {
		t.Fatalf("segment 1 ends %.6fs before the next keyframe, want one frame", gap)
	}
	trailing, keys := segmentPTS(t, dir, 2, "v:0")
	if len(keys) == 0 || math.Abs(keys[0]-next) > 2*integrationTick || trailing[0] < next-2*integrationTick {
		t.Fatalf("the trailing file must start with the keyframe at %.6f, has keys %v", next, keys)
	}
}

func TestTranscodeKeyframesLandOnSourceKeyframes(t *testing.T) {
	fixture := newIntegrationFixture(t, "mp4")
	offset := domain.OutputTimelineOffset.Seconds()
	for _, job := range [][]domain.Segment{fixture.segments[0:2], fixture.segments[1:4]} {
		dir := t.TempDir()
		args, err := integrationBuilder("-preset", "veryfast").Video(VideoParams{InputURL: fixture.path, StreamIndex: 0, Rendition: domain.VideoRendition{Method: domain.Transcode, Width: 320, Height: 180, Bitrate: 400000}, Segments: job, OutputDir: dir})
		if listed := runJob(t, args, err); len(listed) != len(job) {
			t.Fatalf("job listed %v, want %d segments", listed, len(job))
		}
		for _, segment := range job {
			_, keys := segmentPTS(t, dir, segment.Index, "v:0")
			want := segment.Start + offset
			if len(keys) != 1 {
				t.Errorf("segment %d has %d keyframes %v, want one at %.6f", segment.Index, len(keys), keys, want)
				continue
			}
			if math.Abs(keys[0]-want) > integrationFrame/2 {
				t.Errorf("segment %d keyframe at %.6f, source keyframe at %.6f", segment.Index, keys[0], want)
			}
		}
	}
}

func TestAudioJobsJoinExactly(t *testing.T) {
	kinds := []struct {
		name   string
		stream int
		method domain.PlaybackMethod
		frame  float64
	}{
		{"aac", 1, domain.Transcode, 1024.0 / 48000},
		{"ac3", 2, domain.DirectStream, 1536.0 / 48000},
	}
	for _, container := range []string{"mp4", "mkv"} {
		fixture := newIntegrationFixture(t, container)
		for _, kind := range kinds {
			t.Run(container+"/"+kind.name, func(t *testing.T) {
				builder := integrationBuilder()
				rendition := domain.AudioRendition{Method: kind.method, Channels: 2, Bitrate: 128000}
				firstDir, secondDir := t.TempDir(), t.TempDir()

				first := AudioParams{InputURL: fixture.path, StreamIndex: kind.stream, Rendition: rendition, Segments: fixture.segments[0:2], OutputDir: firstDir}
				second := AudioParams{InputURL: fixture.path, StreamIndex: kind.stream, Rendition: rendition, Segments: fixture.segments[2:4], Overlap: &fixture.segments[1], OutputDir: secondDir}
				if kind.method == domain.DirectStream {
					first.Landing = measureLanding(t, builder, fixture.path, 0, kind.stream)
					second.Landing = measureLanding(t, builder, fixture.path, fixture.segments[1].Start, kind.stream)
				}
				args, err := builder.Audio(first)
				if listed := runJob(t, args, err); len(listed) != 2 {
					t.Fatalf("first job listed %v", listed)
				}
				args, err = builder.Audio(second)
				if listed := runJob(t, args, err); len(listed) != 3 {
					t.Fatalf("second job listed %v, want the overlap and two segments", listed)
				}

				offset := domain.OutputTimelineOffset.Seconds()
				var previous []float64
				for index := 0; index < 4; index++ {
					dir := firstDir
					if index >= 2 {
						dir = secondDir
					}
					pts, _ := segmentPTS(t, dir, index, "a:0")
					if diff := pts[0] - (fixture.segments[index].Start + offset); index > 0 && (diff < -2*integrationTick || diff > kind.frame+2*integrationTick) {
						t.Errorf("segment %d starts %.6fs from its boundary, want within one frame after it", index, diff)
					}
					if previous != nil {
						last := previous[len(previous)-1]
						if gap := pts[0] - last; math.Abs(gap-kind.frame) > 2*integrationTick {
							t.Errorf("join %d/%d advances by %.6f, want one frame %.6f", index-1, index, gap, kind.frame)
						}
						seen := make(map[int64]bool, len(previous))
						for _, value := range previous {
							seen[int64(math.Round(value*90000))] = true
						}
						for _, value := range pts {
							if seen[int64(math.Round(value*90000))] {
								t.Errorf("segments %d and %d both contain PTS %.6f", index-1, index, value)
							}
						}
					}
					previous = pts
				}
			})
		}
	}
}

func TestAudioJoinSurvivesBoundaryOnAPacketEdge(t *testing.T) {
	path := testmedia.H264(t, testmedia.Spec{
		Container:      "mp4",
		KeyframeFrames: []int{0, 256, 512},
		Duration:       24,
		Audio:          []testmedia.AudioSpec{{Codec: "aac"}},
	})
	frame := 1024.0 / 48000
	boundary := math.Round(512*1001.0/24000*1e6) / 1e6
	if grid := boundary / frame; math.Abs(grid-math.Round(grid)) > 0.0001 {
		t.Fatalf("fixture boundary %.6f must coincide with an audio packet edge", boundary)
	}
	middle := math.Round(256*1001.0/24000*1e6) / 1e6
	segments := []domain.Segment{{Index: 0, Start: 0, End: middle}, {Index: 1, Start: middle, End: boundary}, {Index: 2, Start: boundary, End: 24}}
	builder := integrationBuilder()
	rendition := domain.AudioRendition{Method: domain.Transcode, Channels: 2, Bitrate: 128000}
	firstDir, secondDir := t.TempDir(), t.TempDir()

	args, err := builder.Audio(AudioParams{InputURL: path, StreamIndex: 1, Rendition: rendition, Segments: segments[0:2], OutputDir: firstDir})
	runJob(t, args, err)
	args, err = builder.Audio(AudioParams{InputURL: path, StreamIndex: 1, Rendition: rendition, Segments: segments[2:3], Overlap: &segments[1], OutputDir: secondDir})
	runJob(t, args, err)

	before, _ := segmentPTS(t, firstDir, 1, "a:0")
	after, _ := segmentPTS(t, secondDir, 2, "a:0")
	if gap := after[0] - before[len(before)-1]; math.Abs(gap-frame) > 2*integrationTick {
		t.Fatalf("join on a packet edge advances by %.6f, want one frame %.6f", gap, frame)
	}
}

func TestTranscodeJobsShareNoFrameAcrossJoins(t *testing.T) {
	for _, container := range []string{"mp4", "mkv"} {
		t.Run(container, func(t *testing.T) {
			fixture := newIntegrationFixture(t, container)
			tolerance := 2 * integrationTick
			if container == "mkv" {
				tolerance = 0.001
			}
			dir := t.TempDir()
			for _, job := range [][]domain.Segment{fixture.segments[0:2], fixture.segments[2:4], fixture.segments[4:5]} {
				args, err := integrationBuilder("-preset", "veryfast").Video(VideoParams{InputURL: fixture.path, StreamIndex: 0, Rendition: domain.VideoRendition{Method: domain.Transcode, Width: 320, Height: 180, Bitrate: 400000}, Segments: job, OutputDir: dir})
				if listed := runJob(t, args, err); len(listed) != len(job) {
					t.Fatalf("job listed %v, want %d segments", listed, len(job))
				}
			}
			offset := domain.OutputTimelineOffset.Seconds()
			var previous []float64
			for index := 0; index < 5; index++ {
				pts, keys := segmentPTS(t, dir, index, "v:0")
				want := fixture.keyframes[0] + offset
				if index > 0 {
					want = fixture.segments[index].Start + offset
				}
				if diff := pts[0] - want; math.Abs(diff) > tolerance {
					t.Errorf("segment %d starts at %.6f, the source frame is at %.6f (off by %+.6f)", index, pts[0], want, diff)
				}
				if len(keys) != 1 || math.Abs(keys[0]-want) > tolerance {
					t.Errorf("segment %d has keyframes %v, want exactly one at %.6f", index, keys, want)
				}
				if previous != nil {
					if gap := pts[0] - previous[len(previous)-1]; math.Abs(gap-integrationFrame) > tolerance {
						t.Errorf("join %d/%d advances by %.6f, want one frame %.6f", index-1, index, gap, integrationFrame)
					}
				}
				previous = pts
			}
		})
	}
}
