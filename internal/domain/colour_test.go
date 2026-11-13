package domain

import "testing"

func TestPlanColour(t *testing.T) {
	cases := []struct {
		name  string
		video VideoStream
		want  ColourPlan
	}{
		{"untagged standard definition", VideoStream{Width: 720, Height: 576}, ColourPlan{AssumeMatrix: "smpte170m"}},
		{"untagged wide standard definition", VideoStream{Width: 1024, Height: 576}, ColourPlan{AssumeMatrix: "smpte170m"}},
		{"untagged 720p", VideoStream{Width: 1280, Height: 720}, ColourPlan{AssumeMatrix: "bt709"}},
		{"untagged tall", VideoStream{Width: 960, Height: 720}, ColourPlan{AssumeMatrix: "bt709"}},
		{"unknown literal", VideoStream{Width: 1920, Height: 1080, ColorSpace: "unknown"}, ColourPlan{AssumeMatrix: "bt709"}},
		{"tagged", VideoStream{Width: 720, Height: 480, ColorSpace: "bt709"}, ColourPlan{}},
		{"hdr10", VideoStream{Width: 3840, Height: 2160, ColorSpace: "bt2020nc", ColorTransfer: "smpte2084", ColorPrimaries: "bt2020"}, ColourPlan{Tonemap: true}},
		{"hlg without matrix or primaries", VideoStream{Width: 3840, Height: 2160, ColorTransfer: "arib-std-b67"}, ColourPlan{Tonemap: true, AssumeMatrix: "bt2020nc", AssumePrimaries: "bt2020"}},
		{"8-bit pq without primaries", VideoStream{Width: 1920, Height: 1080, BitDepth: 8, ColorTransfer: "smpte2084"}, ColourPlan{Tonemap: true, AssumeMatrix: "bt2020nc", AssumePrimaries: "bt2020"}},
		{"8-bit hlg without primaries", VideoStream{Width: 1920, Height: 1080, BitDepth: 8, ColorTransfer: "arib-std-b67", ColorSpace: "bt2020nc"}, ColourPlan{Tonemap: true, AssumePrimaries: "bt2020"}},
		{"8-bit pq with bt709 primaries is mistagged sdr", VideoStream{Width: 1920, Height: 1080, BitDepth: 8, ColorTransfer: "smpte2084", ColorPrimaries: "bt709"}, ColourPlan{Retag: true}},
		{"10-bit pq", VideoStream{Width: 1920, Height: 1080, BitDepth: 10, ColorSpace: "bt2020nc", ColorTransfer: "smpte2084", ColorPrimaries: "bt2020"}, ColourPlan{Tonemap: true}},
		{"unknown depth pq", VideoStream{Width: 1920, Height: 1080, BitDepth: 0, ColorSpace: "bt2020nc", ColorTransfer: "smpte2084", ColorPrimaries: "bt2020"}, ColourPlan{Tonemap: true}},
		{"8-bit sdr", VideoStream{Width: 1920, Height: 1080, BitDepth: 8, ColorSpace: "bt709", ColorTransfer: "bt709"}, ColourPlan{}},
		{"sdr wide gamut", VideoStream{Width: 3840, Height: 2160, ColorSpace: "bt2020nc", ColorTransfer: "bt2020-10", ColorPrimaries: "bt2020"}, ColourPlan{}},
	}
	for _, test := range cases {
		if got := PlanColour(test.video); got != test.want {
			t.Errorf("%s: plan %+v, want %+v", test.name, got, test.want)
		}
	}
}

func TestEightBitPQWithBT2020IsToneMapped(t *testing.T) {
	video := VideoStream{Width: 3840, Height: 2160, BitDepth: 8, PixelFormat: "yuv420p", ColorSpace: "bt2020nc", ColorTransfer: TransferPQ, ColorPrimaries: "bt2020"}
	if got, want := PlanColour(video), (ColourPlan{Tonemap: true}); got != want {
		t.Fatalf("plan %+v, want %+v", got, want)
	}
	if MistaggedHDR(video) {
		t.Fatal("8-bit PQ with bt2020 primaries reported as mis-tagged SDR")
	}
}

func TestEightBitPQWithUnspecifiedPrimariesIsToneMapped(t *testing.T) {
	for _, primaries := range []string{"", "unknown", "reserved"} {
		video := VideoStream{Width: 1920, Height: 1080, BitDepth: 8, ColorSpace: "bt2020nc", ColorTransfer: TransferPQ, ColorPrimaries: primaries}
		if got, want := PlanColour(video), (ColourPlan{Tonemap: true, AssumePrimaries: "bt2020"}); got != want {
			t.Errorf("primaries %q: plan %+v, want %+v", primaries, got, want)
		}
		video.ColorSpace = ""
		if got, want := PlanColour(video), (ColourPlan{Tonemap: true, AssumeMatrix: "bt2020nc", AssumePrimaries: "bt2020"}); got != want {
			t.Errorf("primaries %q without matrix: plan %+v, want %+v", primaries, got, want)
		}
	}
}

func TestEightBitHLGWithBT2020IsToneMapped(t *testing.T) {
	video := VideoStream{Width: 1920, Height: 1080, BitDepth: 8, ColorSpace: "bt2020nc", ColorTransfer: TransferHLG, ColorPrimaries: "bt2020"}
	if got, want := PlanColour(video), (ColourPlan{Tonemap: true}); got != want {
		t.Fatalf("plan %+v, want %+v", got, want)
	}
}

func TestHDRTransferWithSDRPrimariesIsRetagged(t *testing.T) {
	for _, transfer := range []string{TransferPQ, TransferHLG} {
		for _, primaries := range []string{"bt709", "smpte170m", "bt470bg", "bt470m"} {
			for _, depth := range []int{0, 8, 10, 12} {
				video := VideoStream{Width: 1920, Height: 1080, BitDepth: depth, ColorSpace: "bt2020nc", ColorTransfer: transfer, ColorPrimaries: primaries}
				if got, want := PlanColour(video), (ColourPlan{Retag: true}); got != want {
					t.Errorf("%s with %s primaries at %d-bit: plan %+v, want %+v", transfer, primaries, depth, got, want)
				}
				if !MistaggedHDR(video) {
					t.Errorf("%s with %s primaries at %d-bit is not reported as mis-tagged SDR", transfer, primaries, depth)
				}
			}
		}
	}
}

func TestTenBitHDRPlanIsUnchanged(t *testing.T) {
	cases := map[VideoStream]ColourPlan{
		{BitDepth: 10, ColorSpace: "bt2020nc", ColorTransfer: TransferPQ, ColorPrimaries: "bt2020"}:   {Tonemap: true},
		{BitDepth: 10, ColorSpace: "bt2020nc", ColorTransfer: TransferHLG, ColorPrimaries: "bt2020"}:  {Tonemap: true},
		{BitDepth: 10, ColorTransfer: TransferPQ}:                                                     {Tonemap: true, AssumeMatrix: "bt2020nc", AssumePrimaries: "bt2020"},
		{BitDepth: 10, ColorSpace: "bt2020nc", ColorTransfer: TransferHLG, ColorPrimaries: "unknown"}: {Tonemap: true, AssumePrimaries: "bt2020"},
		{BitDepth: 10, ColorSpace: "bt2020nc", ColorTransfer: TransferPQ, ColorPrimaries: "smpte432"}: {Tonemap: true},
		{BitDepth: 12, ColorSpace: "bt2020nc", ColorTransfer: TransferPQ, ColorPrimaries: "bt2020"}:   {Tonemap: true},
	}
	for video, want := range cases {
		if got := PlanColour(video); got != want {
			t.Errorf("%+v: plan %+v, want %+v", video, got, want)
		}
	}
}

func TestBitDepthDoesNotChangeTheHDRPlan(t *testing.T) {
	for _, transfer := range []string{TransferPQ, TransferHLG} {
		for _, primaries := range []string{"", "unknown", "bt2020", "smpte432", "bt709", "smpte170m", "bt470bg", "bt470m"} {
			reference := PlanColour(VideoStream{BitDepth: 10, ColorSpace: "bt2020nc", ColorTransfer: transfer, ColorPrimaries: primaries})
			for _, depth := range []int{0, 8, 12} {
				if got := PlanColour(VideoStream{BitDepth: depth, ColorSpace: "bt2020nc", ColorTransfer: transfer, ColorPrimaries: primaries}); got != reference {
					t.Errorf("%s/%s at %d-bit: plan %+v, 10-bit plan %+v", transfer, primaries, depth, got, reference)
				}
			}
		}
	}
}
