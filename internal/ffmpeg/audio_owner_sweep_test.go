package ffmpeg

import (
	"fmt"
	"math"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/testmedia"
)

var ownerSweepOffsets = []float64{-10000, -3, -2, -1.5, -1, -2.0 / 3, -0.5, -1.0 / 3, 0, 1.0 / 3, 0.5, 2.0 / 3, 1, 1.5, 2, 3, 10000}

func TestAudioJoinOwnsEachFrameOnceAroundTheGrid(t *testing.T) {
	type kind struct {
		name      string
		container string
		codec     string
		rate      int
		method    domain.PlaybackMethod
		frame     float64
	}
	kinds := []kind{
		{"aac48k", "mp4", "aac", 48000, domain.Transcode, 1024.0 / 48000},
		{"aac44k", "mp4", "aac", 44100, domain.Transcode, 1024.0 / 48000},
		{"ac3mp4", "mp4", "ac3", 48000, domain.DirectStream, 1536.0 / 48000},
		{"ac3mkv", "mkv", "ac3", 48000, domain.DirectStream, 1536.0 / 48000},
	}
	for _, kind := range kinds {
		path := testmedia.H264(t, testmedia.Spec{
			Container:      kind.container,
			KeyframeFrames: []int{0, 240, 480, 720},
			Duration:       40,
			Audio:          []testmedia.AudioSpec{{Codec: kind.codec, SampleRate: kind.rate}},
		})
		start := testmedia.FormatStart(t, path)
		grids := []float64{1407 * 1024.0 / 48000, 1408 * 1024.0 / 48000, 1409 * 1024.0 / 48000}
		if kind.method == domain.DirectStream {
			grids = grids[:0]
			for _, packet := range testmedia.Packets(t, path, "a:0") {
				if at := packet.PTS - start; at > 30 && len(grids) < 3 {
					grids = append(grids, at)
				}
			}
		}
		builder := integrationBuilder()
		rendition := domain.AudioRendition{Method: kind.method, Codec: kind.codec, Channels: 2, Bitrate: 128000, SampleRate: kind.rate}
		for _, grid := range grids {
			for _, micros := range ownerSweepOffsets {
				boundary := grid + micros/1e6
				t.Run(fmt.Sprintf("%s/%.6f%+.3fus", kind.name, grid, micros), func(t *testing.T) {
					segments := []domain.Segment{
						{Index: 0, Start: 0, End: 10, Duration: 10},
						{Index: 1, Start: 10, End: 20, Duration: 10},
						{Index: 2, Start: 20, End: boundary, Duration: boundary - 20},
						{Index: 3, Start: boundary, End: 36, Duration: 36 - boundary},
						{Index: 4, Start: 36, End: 40, Duration: 4},
					}
					endDir, startDir := t.TempDir(), t.TempDir()
					ending := AudioParams{InputURL: path, StreamIndex: 1, Rendition: rendition, Segments: segments[2:3], Overlap: &segments[1], OutputDir: endDir}
					ending.Trailing = &segments[3]
					starting := AudioParams{InputURL: path, StreamIndex: 1, Rendition: rendition, Segments: segments[3:4], Overlap: &segments[2], OutputDir: startDir}
					starting.Trailing = &segments[4]
					if kind.method == domain.DirectStream {
						ending.Landing = measureLanding(t, builder, path, segments[1].Start, 1)
						starting.Landing = measureLanding(t, builder, path, segments[2].Start, 1)
					}
					args, err := builder.Audio(ending)
					runJob(t, args, err)
					args, err = builder.Audio(starting)
					runJob(t, args, err)

					before, _ := segmentPTS(t, endDir, 2, "a:0")
					after, _ := segmentPTS(t, startDir, 3, "a:0")
					last, first := before[len(before)-1], after[0]
					if gap := first - last; math.Abs(gap-kind.frame) > 2*integrationTick {
						t.Errorf("join advances by %.6f, want one frame %.6f (last %.6f, first %.6f)", gap, kind.frame, last, first)
					}
					planned := boundary + domain.OutputTimelineOffset.Seconds()
					if first < planned-0.000001-2*integrationTick || first > planned+kind.frame+2*integrationTick {
						t.Errorf("segment 3 starts at %.6f, want the first frame at or after %.6f", first, planned)
					}
				})
			}
		}
	}
}
