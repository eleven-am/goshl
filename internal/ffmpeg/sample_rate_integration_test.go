package ffmpeg

import (
	"fmt"
	"math"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/playlist"
	"github.com/eleven-am/goshl/internal/testmedia"
)

func sampleRateFixture(t *testing.T, sampleRate int, audio []testmedia.AudioSpec) (string, []domain.Segment) {
	t.Helper()
	keyframes := make([]int, 0, 15)
	for frame := 0; frame < 720; frame += 48 {
		keyframes = append(keyframes, frame)
	}
	for index := range audio {
		audio[index].SampleRate = sampleRate
	}
	path := testmedia.H264(t, testmedia.Spec{Container: "mp4", FPS: "24", KeyframeFrames: keyframes, Duration: 30, Audio: audio})
	var points []float64
	start := testmedia.FormatStart(t, path)
	for _, packet := range testmedia.Packets(t, path, "v:0") {
		if packet.Key {
			points = append(points, packet.PTS-start)
		}
	}
	return path, playlist.CalculateSegments(points, 30, 2)
}

func TestAudioJobsJoinExactlyAtEverySourceSampleRate(t *testing.T) {
	for _, sampleRate := range []int{32000, 44100, 48000} {
		t.Run(fmt.Sprint(sampleRate), func(t *testing.T) {
			path, segments := sampleRateFixture(t, sampleRate, []testmedia.AudioSpec{{Codec: "aac"}})
			builder := integrationBuilder()
			rendition := domain.AudioRendition{Method: domain.Transcode, Codec: "aac", Channels: 2, Bitrate: 128000, SampleRate: sampleRate}
			firstDir, secondDir := t.TempDir(), t.TempDir()

			args, err := builder.Audio(AudioParams{InputURL: path, StreamIndex: 1, Rendition: rendition, Segments: segments[0:2], OutputDir: firstDir})
			if listed := runJob(t, args, err); len(listed) != 2 {
				t.Fatalf("first job listed %v", listed)
			}
			args, err = builder.Audio(AudioParams{InputURL: path, StreamIndex: 1, Rendition: rendition, Segments: segments[2:4], Overlap: &segments[1], OutputDir: secondDir})
			if listed := runJob(t, args, err); len(listed) != 3 {
				t.Fatalf("second job listed %v, want the overlap and two segments", listed)
			}

			frame := 1024.0 / 48000
			first, _ := segmentPTS(t, firstDir, 1, "a:0")
			second, _ := segmentPTS(t, secondDir, 2, "a:0")
			if gap := second[0] - first[len(first)-1]; math.Abs(gap-frame) > 2*integrationTick {
				t.Errorf("join 1/2 advances by %.6f, want one frame %.6f", gap, frame)
			}
			boundary := segments[2].Start + domain.OutputTimelineOffset.Seconds()
			if diff := second[0] - boundary; diff < -2*integrationTick || diff > frame+2*integrationTick {
				t.Errorf("segment 2 starts %.6fs from its boundary, want within one frame after it", diff)
			}
		})
	}
}

func TestPassthroughTailJoinsAtSourceSampleRate(t *testing.T) {
	const sampleRate = 44100
	path, segments := sampleRateFixture(t, sampleRate, []testmedia.AudioSpec{{Codec: "ac3", Duration: 17.5}})
	builder := integrationBuilder()
	rendition := domain.AudioRendition{Method: domain.DirectStream, Codec: "ac3", Channels: 2, SampleRate: sampleRate, SourceEnd: 17.5}
	frame := 1536.0 / sampleRate

	var dirs []string
	var previous []float64
	for _, bounds := range [][2]int{{0, 3}, {4, 7}, {8, 10}} {
		dir := t.TempDir()
		dirs = append(dirs, dir)
		params := AudioParams{InputURL: path, StreamIndex: 1, Rendition: rendition, Segments: segments[bounds[0] : bounds[1]+1], OutputDir: dir}
		if bounds[0] > 0 {
			params.Overlap = &segments[bounds[0]-1]
		}
		params.Landing = measureLanding(t, builder, path, jobStart(params), 1)
		args, err := builder.Audio(params)
		runJob(t, args, err)
		pts, _ := segmentPTS(t, dir, bounds[0], "a:0")
		if previous != nil {
			if gap := pts[0] - previous[len(previous)-1]; math.Abs(gap-frame) > 2*integrationTick {
				t.Errorf("job at segment %d joins by %.6f, want one frame %.6f", bounds[0], gap, frame)
			}
		}
		previous, _ = segmentPTS(t, dir, bounds[1], "a:0")
	}
	if len(dirs) != 3 {
		t.Fatalf("expected three jobs")
	}
}

func jobStart(params AudioParams) float64 {
	if params.Overlap != nil {
		return params.Overlap.Start
	}
	return params.Segments[0].Start
}
