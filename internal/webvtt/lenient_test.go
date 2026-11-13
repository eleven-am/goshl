package webvtt

import (
	"testing"
	"time"
)

func TestParseKeepsCueTextThatContainsABlankLine(t *testing.T) {
	input := "WEBVTT\n\n00:00.072 --> 00:00.672\nNormal cue\n\n00:00.872 --> 00:01.372\nSpeaker one\n\nSpeaker two\nsecond line\n\n00:01.472 --> 00:01.772\nLast cue\n"
	cues, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("a blank line inside one cue failed the whole track: %v", err)
	}
	if len(cues) != 3 {
		t.Fatalf("got %d cues, want 3: %+v", len(cues), cues)
	}
	if cues[1].Text != "Speaker one\nSpeaker two\nsecond line" || cues[1].Start != 872*time.Millisecond {
		t.Fatalf("second cue = %+v", cues[1])
	}
	if cues[2].Text != "Last cue" {
		t.Fatalf("third cue = %+v", cues[2])
	}
}

func TestParseDropsOnlyTheCueWithUnreadableTiming(t *testing.T) {
	input := "WEBVTT\n\n00:00.000 --> 00:01.000\nfirst\n\n00:0x.000 --> 00:02.000\nbroken\n\n00:03.000 --> 00:04.000\nthird\n"
	cues, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("one unreadable timing line failed the whole track: %v", err)
	}
	if len(cues) != 2 || cues[0].Text != "first" || cues[1].Text != "third" {
		t.Fatalf("cues = %+v", cues)
	}
}

func TestParseStillRejectsInputWithoutSignature(t *testing.T) {
	if _, err := Parse([]byte("00:00.000 --> 00:01.000\nfirst\n")); err == nil {
		t.Fatal("input without the WEBVTT signature was accepted")
	}
}
