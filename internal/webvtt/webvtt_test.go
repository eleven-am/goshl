package webvtt

import (
	"strings"
	"testing"
	"time"
)

func TestTimestampRoundsToNearestMillisecond(t *testing.T) {
	cases := map[float64]string{
		0:          "00:00:00.000",
		1.001:      "00:00:01.001",
		7.3:        "00:00:07.300",
		3661.789:   "01:01:01.789",
		0.0004:     "00:00:00.000",
		0.0006:     "00:00:00.001",
		59.9996:    "00:01:00.000",
		360000.5:   "100:00:00.500",
		-3:         "00:00:00.000",
		12.3456789: "00:00:12.346",
	}
	for seconds, want := range cases {
		if got := Timestamp(seconds); got != want {
			t.Errorf("Timestamp(%v) = %s, want %s", seconds, got, want)
		}
	}
	if got := FormatDuration(1999*time.Millisecond + 600*time.Microsecond); got != "00:00:02.000" {
		t.Errorf("FormatDuration rounds to %s, want 00:00:02.000", got)
	}
}

func TestCueTextEscapesOnlyVTTSpecialCharacters(t *testing.T) {
	got := EscapeText(`Tom's "quote" & <b>bold</b> > done`)
	want := `Tom's "quote" &amp; &lt;b&gt;bold&lt;/b&gt; &gt; done`
	if got != want {
		t.Fatalf("EscapeText = %q, want %q", got, want)
	}
}

func TestCueTextCannotTerminateCueEarly(t *testing.T) {
	cues := []Cue{
		{ID: "id\n00:00:00.000 --> 00:00:01.000", Start: time.Second, End: 2 * time.Second, Settings: "align:start\n\nline:0 --> x", Text: "first\n\n\nsecond --> third\r\n \r\nfourth"},
		{Start: 3 * time.Second, End: 4 * time.Second, Text: "next"},
	}
	encoded := Encode(cues, Options{})
	parsed, err := Parse(encoded)
	if err != nil {
		t.Fatalf("parse encoded output: %v\n%s", err, encoded)
	}
	if len(parsed) != 2 {
		t.Fatalf("cue count = %d, want 2:\n%s", len(parsed), encoded)
	}
	if parsed[0].Text != "first\nsecond --&gt; third\nfourth" {
		t.Errorf("first cue text = %q", parsed[0].Text)
	}
	if strings.Count(string(encoded), "-->") != 2 {
		t.Errorf("output contains stray arrows:\n%s", encoded)
	}
	if strings.Contains(parsed[0].ID, "\n") || strings.Contains(parsed[0].Settings, "\n") {
		t.Errorf("id or settings span lines: %+v", parsed[0])
	}
}

func TestEncodeTimestampMapHeader(t *testing.T) {
	cues := []Cue{{Start: time.Second, End: 2 * time.Second, Text: "hi"}}
	withMap := string(Encode(cues, Options{TimestampMap: true}))
	if !strings.HasPrefix(withMap, "WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:900000,LOCAL:00:00:00.000\n\n00:00:01.000 --> 00:00:02.000\nhi\n\n") {
		t.Errorf("unexpected header:\n%s", withMap)
	}
	plain := string(Encode(cues, Options{}))
	if strings.Contains(plain, "X-TIMESTAMP-MAP") || !strings.HasPrefix(plain, "WEBVTT\n\n") {
		t.Errorf("plain output carries a map:\n%s", plain)
	}
}

func TestShiftClampsAndDropsCuesBeforeOrigin(t *testing.T) {
	cues := []Cue{
		{Start: 500 * time.Millisecond, End: 900 * time.Millisecond, Text: "gone"},
		{Start: 800 * time.Millisecond, End: 2 * time.Second, Text: "clamped"},
		{Start: 3 * time.Second, End: 4 * time.Second, Text: "kept"},
	}
	shifted := Shift(cues, time.Second)
	if len(shifted) != 2 || shifted[0].Start != 0 || shifted[0].End != time.Second || shifted[1].Start != 2*time.Second {
		t.Fatalf("shifted = %+v", shifted)
	}
}

func TestParseFFmpegOutput(t *testing.T) {
	input := "\xef\xbb\xbfWEBVTT\r\n\r\nNOTE made by ffmpeg\r\n\r\n1\r\n00:00:01.000 --> 00:00:02.500 align:start\r\nHello &amp; <i>bye</i>\r\nsecond\r\n\r\n01:00:03.000 --> 01:00:04.000\r\nlate\r\n\r\n"
	cues, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cues) != 2 {
		t.Fatalf("cue count = %d", len(cues))
	}
	if cues[0].ID != "1" || cues[0].Start != time.Second || cues[0].End != 2500*time.Millisecond || cues[0].Settings != "align:start" || cues[0].Text != "Hello &amp; <i>bye</i>\nsecond" {
		t.Errorf("first cue = %+v", cues[0])
	}
	if cues[1].Start != time.Hour+3*time.Second {
		t.Errorf("second cue start = %v", cues[1].Start)
	}
}

func TestParseRejectsMalformedInput(t *testing.T) {
	for name, input := range map[string]string{
		"no signature":   "00:00:01.000 --> 00:00:02.000\nhi\n",
		"empty":          "",
		"bad timestamp":  "WEBVTT\n\n00:00:01 --> 00:00:02.000\nhi\n",
		"bad end":        "WEBVTT\n\n00:00:01.000 --> nonsense\nhi\n",
		"orphan payload": "WEBVTT\n\nhello\nworld\n",
		"minute range":   "WEBVTT\n\n00:75:01.000 --> 00:76:02.000\nhi\n",
	} {
		if _, err := Parse([]byte(input)); err == nil {
			t.Errorf("%s: parse succeeded", name)
		}
	}
}

func TestParseThenEncodeRoundTrips(t *testing.T) {
	input := "WEBVTT\n\nid-1\n00:00:01.000 --> 00:00:02.000 line:0\nHello\n\n00:00:03.000 --> 00:00:04.000\nWorld\n\n"
	cues, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := string(Encode(cues, Options{})); got != input {
		t.Errorf("round trip = %q, want %q", got, input)
	}
}
