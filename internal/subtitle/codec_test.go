package subtitle

import "testing"

func TestConvertibleCodecs(t *testing.T) {
	for codec, kind := range codecKinds {
		if got := KindOf(codec); got != kind {
			t.Errorf("KindOf(%q) = %v, want %v", codec, got, kind)
		}
		if want := kind == KindText; Convertible(codec) != want {
			t.Errorf("Convertible(%q) = %v, want %v", codec, Convertible(codec), want)
		}
	}
	for _, codec := range []string{"subrip", "ass", "mov_text", "webvtt", "eia_608"} {
		if !Convertible(codec) {
			t.Errorf("text codec %q must be convertible", codec)
		}
	}
	for _, codec := range []string{"hdmv_pgs_subtitle", "pgssub", "dvd_subtitle", "dvdsub", "dvb_subtitle", "dvbsub", "dvb_teletext", "xsub", "", "ttml", "hdmv_text_subtitle", "arib_caption", "unknown"} {
		if Convertible(codec) {
			t.Errorf("codec %q must not be convertible", codec)
		}
	}
	for _, codec := range []string{"", "ttml", "hdmv_text_subtitle", "arib_caption", "unknown"} {
		if KindOf(codec) != KindUnknown {
			t.Errorf("KindOf(%q) must be KindUnknown", codec)
		}
	}
}
