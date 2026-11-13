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
		{"8-bit pq is mistagged sdr", VideoStream{Width: 1920, Height: 1080, BitDepth: 8, ColorTransfer: "smpte2084"}, ColourPlan{Retag: true}},
		{"8-bit hlg is mistagged sdr", VideoStream{Width: 1920, Height: 1080, BitDepth: 8, ColorTransfer: "arib-std-b67", ColorSpace: "bt2020nc"}, ColourPlan{Retag: true}},
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
