package ffmpeg

import (
	"math"
	"sort"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/playlist"
	"github.com/eleven-am/goshl/internal/testmedia"
)

func splitPlanFixture(t *testing.T, container string) (string, []float64, []domain.Segment, []float64) {
	path := testmedia.H264(t, testmedia.Spec{Container: container, BFrames: 3, Pyramid: true, KeyframeFrames: []int{0, 480, 530}, Duration: 24, Audio: []testmedia.AudioSpec{{Codec: "aac"}}})
	start := testmedia.FormatStart(t, path)
	var keyframes, frames []float64
	for _, packet := range testmedia.Packets(t, path, "v:0") {
		frames = append(frames, packet.PTS-start)
		if packet.Key {
			keyframes = append(keyframes, packet.PTS-start)
		}
	}
	sort.Float64s(keyframes)
	sort.Float64s(frames)
	t.Logf("keyframes %v", keyframes)
	segments := playlist.CalculateSegments(keyframes, 24, 2)
	if playlist.KeyframeAligned(keyframes, 24, 2) {
		t.Fatalf("fixture must need a split")
	}
	return path, keyframes, segments, frames
}

func TestSplitPlanTranscodeCoversEveryFrame(t *testing.T) {
	for _, container := range []string{"mp4", "mkv"} {
		t.Run(container, func(t *testing.T) {
			path, _, segments, frames := splitPlanFixture(t, container)
			t.Logf("plan %v", segments)
			dir := t.TempDir()
			for begin := 0; begin < len(segments); begin += 4 {
				end := begin + 4
				if end > len(segments) {
					end = len(segments)
				}
				job := segments[begin:end]
				args, err := integrationBuilder("-preset", "veryfast").Video(VideoParams{InputURL: path, StreamIndex: 0, Rendition: domain.VideoRendition{Method: domain.Transcode, Width: 320, Height: 180, Bitrate: 400000, PeakBitrate: 600000, BufferSize: 1200000, Level: 30}, Segments: job, OutputDir: dir})
				if listed := runJob(t, args, err); len(listed) != len(job) {
					t.Fatalf("job %d listed %v, want %d segments", begin, listed, len(job))
				}
			}
			offset := domain.OutputTimelineOffset.Seconds()
			var all []float64
			for index, segment := range segments {
				pts, keys := segmentPTS(t, dir, index, "v:0")
				if len(keys) != 1 || keys[0] != pts[0] {
					t.Errorf("segment %d keyframes %v first %v", index, keys, pts[0])
				}
				drift := pts[0] - offset - segment.Start
				if index > 0 && (drift < -0.002 || drift > integrationFrame+0.002) {
					t.Errorf("segment %d starts %.6f after planned start %.6f", index, drift, segment.Start)
				}
				measured := float64(len(pts)) * integrationFrame
				if math.Abs(measured-segment.Duration) > integrationFrame+0.002 {
					t.Errorf("segment %d holds %.6f s, planned %.6f", index, measured, segment.Duration)
				}
				all = append(all, pts...)
			}
			if len(all) != len(frames) {
				t.Fatalf("output has %d frames, source %d", len(all), len(frames))
			}
			for i := range all {
				if math.Abs(all[i]-offset-frames[i]) > 0.002 {
					t.Fatalf("frame %d at %.6f, source %.6f", i, all[i]-offset, frames[i])
				}
			}
		})
	}
}

func TestSplitPlanAudioYieldsEverySegment(t *testing.T) {
	path, _, segments, _ := splitPlanFixture(t, "mp4")
	dir := t.TempDir()
	job := segments[2:6]
	args, err := integrationBuilder().Audio(AudioParams{InputURL: path, StreamIndex: 1, Rendition: domain.AudioRendition{Method: domain.Transcode, Channels: 2, Bitrate: 128000}, Segments: job, Overlap: &segments[1], OutputDir: dir})
	listed := runJob(t, args, err)
	t.Logf("audio listed %v", listed)
	if len(listed) != len(job)+1 {
		t.Fatalf("audio job listed %v, want %d", listed, len(job)+1)
	}
}

func TestEarlyCutPlanCopiesEveryFrame(t *testing.T) {
	path := testmedia.H264(t, testmedia.Spec{Container: "mp4", BFrames: 3, Pyramid: true, KeyframeFrames: []int{0, 15, 290, 300}, Duration: 16})
	var keyframes, frames []float64
	for _, packet := range testmedia.Packets(t, path, "v:0") {
		frames = append(frames, packet.PTS)
		if packet.Key {
			keyframes = append(keyframes, packet.PTS)
		}
	}
	sort.Float64s(keyframes)
	sort.Float64s(frames)
	segments := playlist.CalculateSegments(keyframes, 16, 6)
	t.Logf("keyframes %v plan %v", keyframes, segments)
	if !playlist.KeyframeAligned(keyframes, 16, 6) || len(segments) != 3 {
		t.Fatalf("fixture must plan three keyframe aligned segments")
	}
	builder := integrationBuilder()
	dir := t.TempDir()
	first, err := builder.Video(VideoParams{InputURL: path, StreamIndex: 0, Rendition: domain.VideoRendition{Method: domain.DirectStream}, Segments: segments[0:1], Trailing: &segments[1], Landing: measureLanding(t, builder, path, 0, 0), OutputDir: dir})
	t.Logf("first job listed %v", runJob(t, first, err))
	second, err := builder.Video(VideoParams{InputURL: path, StreamIndex: 0, Rendition: domain.VideoRendition{Method: domain.DirectStream}, Segments: segments[1:3], Overlap: &segments[0], Landing: measureLanding(t, builder, path, 0, 0), OutputDir: dir})
	t.Logf("second job listed %v", runJob(t, second, err))
	var all []float64
	for index := range segments {
		pts, keys := segmentPTS(t, dir, index, "v:0")
		if len(keys) == 0 || math.Abs(pts[0]-10-segments[index].Start) > 0.001 {
			t.Errorf("segment %d starts at %.6f, planned %.6f", index, pts[0]-10, segments[index].Start)
		}
		all = append(all, pts...)
	}
	if len(all) != len(frames) {
		t.Fatalf("copied %d frames, source has %d", len(all), len(frames))
	}
}
