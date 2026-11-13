package rendition

import (
	"math"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/h264level"
)

func directSource() domain.VideoStream {
	return domain.VideoStream{Codec: "h264", Width: 1920, Height: 1080, Bitrate: 5_000_000, FrameRate: 24, PixelFormat: "yuv420p", Profile: "High", Level: 40, StartsWithKeyframe: true}
}

func measuredMeta(bytes []int64) *domain.Metadata {
	return &domain.Metadata{
		Duration:      36,
		Keyframes:     []float64{0, 6, 12, 18, 24, 30},
		KeyframeBytes: bytes,
		Video:         directSource(),
	}
}

func TestDirectPeakBandwidthUsesHeaviestWindow(t *testing.T) {
	meta := measuredMeta([]int64{10_000, 10_000, 6_000_000, 10_000, 10_000, 10_000})
	window := planWindow(meta, 6)

	got := directPeakBandwidth(meta, window)

	heavy := transportBits(6_000_000, math.Ceil(24*6)+1, 6) / 6
	if got < int(heavy) {
		t.Fatalf("peak %d is below the heaviest segment %d", got, int(heavy))
	}
	if got > int(heavy*1.01) {
		t.Fatalf("peak %d is far above the heaviest segment %d", got, int(heavy))
	}
	average := transportBits(6_050_000, 24*36+1, 36) / 36
	if float64(got) < 3*average {
		t.Fatalf("peak %d does not exceed the average %d by the burst ratio", got, int(average))
	}
}

func TestDirectPeakBandwidthIgnoresWindowsShorterThanHalfTarget(t *testing.T) {
	meta := &domain.Metadata{
		Duration:      11,
		Keyframes:     []float64{0, 10},
		KeyframeBytes: []int64{100_000, 900_000},
		Video:         directSource(),
	}
	window := peakWindow{
		segments: []domain.Segment{{Index: 0, Start: 0, End: 10, Duration: 10}, {Index: 1, Start: 10, End: 11, Duration: 1}},
		shortest: 5,
		longest:  15,
	}

	got := directPeakBandwidth(meta, window)

	first := transportBits(100_000, 24*10+1, 10)
	second := transportBits(900_000, 24*1+1, 1)
	want := int(math.Ceil((first + second) / 11))
	if got != want {
		t.Fatalf("peak = %d, want the combined window %d, not the lone short segment %d", got, want, int(second))
	}
}

func TestDirectPeakBandwidthWithoutByteFactsCannotUnderAdvertise(t *testing.T) {
	for name, bytes := range map[string][]int64{"empty": nil, "wrong length": {1, 2}} {
		meta := measuredMeta(bytes)
		window := planWindow(meta, 6)

		got := directPeakBandwidth(meta, window)

		if got < h264level.MaxCPB(40) {
			t.Fatalf("%s: peak %d is below the level 4.0 ceiling %d", name, got, h264level.MaxCPB(40))
		}
	}
}

func TestGenerateVideoDirectStreamAdvertisesMeasuredPeak(t *testing.T) {
	meta := measuredMeta([]int64{10_000, 10_000, 6_000_000, 10_000, 10_000, 10_000})

	renditions, _, err := GenerateVideo(meta, 6)
	if err != nil {
		t.Fatalf("GenerateVideo: %v", err)
	}

	top := renditions[0]
	if top.Method != domain.DirectStream {
		t.Fatalf("top rung method = %s, want direct stream", top.Method)
	}
	if want := directPeakBandwidth(meta, planWindow(meta, 6)); top.PeakBandwidth != want {
		t.Fatalf("PeakBandwidth = %d, want %d", top.PeakBandwidth, want)
	}
	if top.PeakBandwidth < 7_500_000 {
		t.Fatalf("PeakBandwidth = %d, below the real burst of about 8 Mb/s", top.PeakBandwidth)
	}
}

func TestGenerateVideoTranscodedRungsCoverRateControlBound(t *testing.T) {
	meta := measuredMeta(nil)
	meta.Video.Level = 0

	renditions, _, err := GenerateVideo(meta, 6)
	if err != nil {
		t.Fatalf("GenerateVideo: %v", err)
	}

	for _, rung := range renditions {
		if rung.Method != domain.Transcode {
			t.Fatalf("%s: method = %s, want transcode", rung.Name, rung.Method)
		}
		bound := rung.PeakBitrate + int(float64(rung.BufferSize)/3)
		if rung.PeakBandwidth < bound {
			t.Errorf("%s: PeakBandwidth %d is below maxrate plus bufsize over the shortest window %d", rung.Name, rung.PeakBandwidth, bound)
		}
		if rung.PeakBandwidth > 2*bound {
			t.Errorf("%s: PeakBandwidth %d is more than twice the bound %d", rung.Name, rung.PeakBandwidth, bound)
		}
	}
}

func TestAudioRenditionsAdvertisePeakBandwidth(t *testing.T) {
	renditions := GenerateAudioTracks([]domain.AudioStream{domain.AudioStream{Codec: "ac3", Channels: 6, Bitrate: 448_000, SampleRate: 48000}})

	for _, rendition := range renditions {
		if rendition.PeakBandwidth < rendition.Bitrate {
			t.Errorf("%s: PeakBandwidth %d below nominal bitrate %d", rendition.Name, rendition.PeakBandwidth, rendition.Bitrate)
		}
		if rendition.Name == "aac_stereo" && rendition.PeakBandwidth < 199_000 {
			t.Errorf("aac_stereo PeakBandwidth = %d, want at least the 199 kb/s measured on the wire", rendition.PeakBandwidth)
		}
	}
}

func TestMeanTransportRateModel(t *testing.T) {
	got := meanTransportRate(125_000, 24)
	want := int(math.Ceil((125_000.0*188/184 + 24*120 + 4700) * 8))
	if got != want {
		t.Fatalf("mean rate %d, want %d", got, want)
	}
}

func TestEncodedAverageStaysBelowPeak(t *testing.T) {
	renditions := mustVideo(t, directSource())
	for _, rendition := range renditions {
		if rendition.AverageBandwidth <= rendition.Bitrate {
			t.Errorf("%s average %d does not include transport overhead over %d", rendition.Name, rendition.AverageBandwidth, rendition.Bitrate)
		}
		if rendition.AverageBandwidth > rendition.PeakBandwidth {
			t.Errorf("%s average %d exceeds peak %d", rendition.Name, rendition.AverageBandwidth, rendition.PeakBandwidth)
		}
	}
}

func TestDirectAverageUsesStreamBitrateAndStripsAvc1ParameterSets(t *testing.T) {
	source := directSource()
	source.CodecTag = "avc1"
	direct := mustVideo(t, source)[0]
	if direct.Method != domain.DirectStream || !direct.StripParameterSets {
		t.Fatalf("avc1 direct rung %+v must strip parameter sets", direct)
	}
	if want := encodedAverageBandwidth(direct); direct.AverageBandwidth != min(want, direct.PeakBandwidth) {
		t.Fatalf("direct average %d, want %d", direct.AverageBandwidth, want)
	}
	source.CodecTag = "avc3"
	if mustVideo(t, source)[0].StripParameterSets {
		t.Fatalf("avc3 source must keep in-band parameter sets")
	}
}

func TestAudioAverageBandwidth(t *testing.T) {
	encoded := audioAverageBandwidth(128_000, 48000, domain.Transcode)
	if encoded <= 128_000 || encoded >= audioPeakBandwidth(128_000, 48000, domain.Transcode) {
		t.Fatalf("encoded audio average %d outside (bitrate, peak)", encoded)
	}
	direct := audioAverageBandwidth(384_000, 48000, domain.DirectStream)
	if direct <= 384_000 {
		t.Fatalf("direct audio average %d must exceed payload bitrate", direct)
	}
}
