package ffmpeg

import (
	"errors"
	"strings"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
)

func copyVideoParams(strip bool) VideoParams {
	return copyVideoParamsWith(strip, false)
}

func copyVideoParamsWith(stripParameterSets, stripSEI bool) VideoParams {
	return VideoParams{
		InputURL:  "input.mp4",
		Rendition: domain.VideoRendition{Method: domain.DirectStream, Width: 1920, Height: 1080, Bitrate: 8_000_000, StripParameterSets: stripParameterSets, StripSEI: stripSEI},
		Segments:  []domain.Segment{{Index: 5, Start: 30, End: 36}},
		OutputDir: "/tmp/out",
		Landing:   landing(29.5),
	}
}

func TestVideoCommand_Avc1CopyStripsInBandParameterSets(t *testing.T) {
	args, err := NewCommandBuilder(testHW).Video(copyVideoParams(true))
	joined := mustArgs(t, args, err)
	if !strings.Contains(joined, "-bsf:v filter_units=remove_types=7-8,h264_mp4toannexb,dump_extra") {
		t.Fatalf("avc1 copy must strip in-band SPS and PPS before re-inserting them: %s", joined)
	}

	args, err = NewCommandBuilder(testHW).Video(copyVideoParams(false))
	joined = mustArgs(t, args, err)
	if strings.Contains(joined, "filter_units") || !strings.Contains(joined, "-bsf:v h264_mp4toannexb,dump_extra") {
		t.Fatalf("other copy sources must keep the plain filter chain: %s", joined)
	}
}

func TestVideoCommand_CopyRemovedUnitTypesFollowFlags(t *testing.T) {
	cases := []struct {
		parameterSets, sei bool
		want               string
	}{
		{false, false, "-bsf:v h264_mp4toannexb,dump_extra"},
		{true, false, "-bsf:v filter_units=remove_types=7-8,h264_mp4toannexb,dump_extra"},
		{false, true, "-bsf:v filter_units=remove_types=6,h264_mp4toannexb,dump_extra"},
		{true, true, "-bsf:v filter_units=remove_types=6|7-8,h264_mp4toannexb,dump_extra"},
	}
	for _, c := range cases {
		args, err := NewCommandBuilder(testHW).Video(copyVideoParamsWith(c.parameterSets, c.sei))
		joined := mustArgs(t, args, err)
		if !strings.Contains(joined, c.want) {
			t.Fatalf("parameterSets=%v sei=%v: want %q in %s", c.parameterSets, c.sei, c.want, joined)
		}
	}
}

func TestVideoCommand_RetagDeclaresBT709(t *testing.T) {
	params := VideoParams{
		InputURL:  "input.mp4",
		Rendition: domain.VideoRendition{Method: domain.Transcode, Width: 1280, Height: 720, Bitrate: 2_000_000, PeakBitrate: 3_000_000, BufferSize: 6_000_000, Level: 31, Colour: domain.ColourPlan{Retag: true}},
		Segments:  []domain.Segment{{Index: 0, Start: 0, End: 6}},
		OutputDir: "/tmp/out",
	}
	args, err := NewCommandBuilder(testHW).Video(params)
	joined := mustArgs(t, args, err)
	if !strings.Contains(joined, "setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709") {
		t.Fatalf("retag must declare bt709 on the frames: %s", joined)
	}
	if !strings.Contains(joined, "-color_primaries bt709 -color_trc bt709 -colorspace bt709") {
		t.Fatalf("retag must declare bt709 on the encoder: %s", joined)
	}

	params.Rendition.Colour = domain.ColourPlan{}
	args, err = NewCommandBuilder(testHW).Video(params)
	joined = mustArgs(t, args, err)
	if strings.Contains(joined, "setparams") || strings.Contains(joined, "-color_trc") {
		t.Fatalf("an untagged plan must not retag: %s", joined)
	}
}

func TestAudioCommand_TrailingSegmentExtendsTheJobAndAddsACut(t *testing.T) {
	segments := []domain.Segment{{Index: 1, Start: 5, End: 10}}
	trailing := domain.Segment{Index: 2, Start: 10, End: 15}
	aac := AudioParams{
		InputURL:  "in.mkv",
		Rendition: domain.AudioRendition{Method: domain.Transcode, Channels: 2, Bitrate: 192000},
		Segments:  segments,
		Overlap:   &domain.Segment{Index: 0, Start: 0, End: 5},
		Trailing:  &trailing,
		OutputDir: "/tmp/a",
	}
	args, err := NewCommandBuilder(testHW).Audio(aac)
	joined := mustArgs(t, args, err)
	if cuts := strings.Split(optionValue(t, joined, "-segment_times"), ","); len(cuts) != 2 {
		t.Fatalf("the segment start and the trailing start must each be cut, got %v", cuts)
	}
	if !strings.Contains(joined, "-to 11.000000") {
		t.Fatalf("the job must read one guard second past the trailing start: %s", joined)
	}

	copied := aac
	copied.Rendition = domain.AudioRendition{Method: domain.DirectStream}
	copied.Landing = landing(4.9)
	args, err = NewCommandBuilder(testHW).Audio(copied)
	joined = mustArgs(t, args, err)
	if !strings.Contains(joined, "-to 11.000000") || !strings.Contains(joined, "-c:a copy") {
		t.Fatalf("copied audio must read past the trailing start: %s", joined)
	}

	aac.Trailing = &domain.Segment{Index: 3, Start: 10, End: 15}
	if _, err := NewCommandBuilder(testHW).Audio(aac); !errors.Is(err, ErrTrailingNotAdjacent) {
		t.Fatalf("a non adjacent trailing segment must be rejected, got %v", err)
	}
}

func TestAACOwnerFrameFollowsTheMicrosecondRule(t *testing.T) {
	frame := 1024.0 / 48000
	for _, test := range []struct {
		boundary float64
		want     int64
	}{
		{0, 0},
		{10 * frame, 10},
		{10*frame + 0.000002, 11},
		{10*frame + 0.0000005, 10},
		{10*frame + 0.005, 11},
	} {
		if got := aacOwnerFrame(test.boundary); got != test.want {
			t.Errorf("boundary %.9f owner frame %d, want %d", test.boundary, got, test.want)
		}
	}
	for _, boundary := range []float64{5, 10 * frame, 99.999999} {
		owner := float64(aacOwnerFrame(boundary)) * frame
		if distance := owner - aacCutTime(boundary); distance < frame/2-1e-9 || distance > frame/2+1e-9 {
			t.Errorf("cut for %.6f is %.9f from the owner frame start, want half a frame", boundary, distance)
		}
	}
}
