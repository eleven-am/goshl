package subtitle

import (
	"bytes"
	"context"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/eleven-am/goshl/internal/webvtt"
)

const wvttSampleTicks = 2000000

func wvttSample(t *testing.T, id, settings, text string) []byte {
	t.Helper()
	cue := &mp4.VttcBox{}
	if id != "" {
		cue.AddChild(&mp4.IdenBox{CueID: id})
	}
	if settings != "" {
		cue.AddChild(&mp4.SttgBox{Settings: settings})
	}
	cue.AddChild(&mp4.PaylBox{CueText: text})
	var encoded bytes.Buffer
	if err := cue.Encode(&encoded); err != nil {
		t.Fatalf("encode wvtt sample: %v", err)
	}
	return encoded.Bytes()
}

func wvttSource(t *testing.T, samples [][]byte) []byte {
	t.Helper()
	build := func(offset uint32) []byte {
		return craftedSubtitleSource(t, func(stbl *mp4.StblBox) {
			entry := mp4.NewWvttBox()
			entry.AddChild(&mp4.VttCBox{Config: "WEBVTT"})
			stbl.Stsd.Children = nil
			stbl.Stsd.AddChild(entry)
			stbl.Stsd.SampleCount = 1
			stbl.Stts.SampleCount = []uint32{uint32(len(samples))}
			stbl.Stts.SampleTimeDelta = []uint32{wvttSampleTicks}
			stbl.Ctts = nil
			stbl.Stsz.SampleUniformSize = 0
			stbl.Stsz.SampleNumber = uint32(len(samples))
			stbl.Stsz.SampleSize = nil
			for _, sample := range samples {
				stbl.Stsz.SampleSize = append(stbl.Stsz.SampleSize, uint32(len(sample)))
			}
			stbl.Stsc.Entries = []mp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: uint32(len(samples)), FirstSampleNr: 1}}
			stbl.Stsc.SetSingleSampleDescriptionID(1)
			stbl.Stco.ChunkOffset = []uint32{offset}
		})
	}
	const padding = 64 << 10
	payloadAt := len(build(0)) - padding
	data := build(uint32(payloadAt))
	if len(data)-padding != payloadAt {
		t.Fatalf("payload offset moved from %d to %d", payloadAt, len(data)-padding)
	}
	at := payloadAt
	for _, sample := range samples {
		at += copy(data[at:], sample)
	}
	return data
}

func TestExtractBatchConvertsWebVTTSampleEntries(t *testing.T) {
	data := wvttSource(t, [][]byte{
		wvttSample(t, "c1", "align:start", "First wvtt"),
		wvttSample(t, "", "", "Second <i>wvtt</i>"),
	})
	tracks := []Track{{ID: "native", StreamIndex: 1, Codec: "webvtt"}, {ID: "tagged", StreamIndex: 1, Codec: "wvtt"}, {ID: "other", StreamIndex: 1, Codec: "mov_text"}}
	outcomes, err := extractBatchFrom(context.Background(), bytes.NewReader(data), int64(len(data)), tracks)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if outcomes["other"].Supported {
		t.Fatal("a mov_text request must not be served from a wvtt sample entry")
	}
	for _, id := range []string{"native", "tagged"} {
		outcome := outcomes[id]
		if !outcome.Supported || outcome.Err != nil {
			t.Fatalf("%s: supported %v, err %v", id, outcome.Supported, outcome.Err)
		}
		cues, err := webvtt.Parse(outcome.Data)
		if err != nil || len(cues) != 2 {
			t.Fatalf("%s: %d cues, err %v:\n%s", id, len(cues), err, outcome.Data)
		}
		if cues[0].ID != "c1" || cues[0].Settings != "align:start" || cues[0].Text != "First wvtt" || cues[1].ID != "" || cues[1].Text != "Second <i>wvtt</i>" {
			t.Fatalf("%s: cues = %+v", id, cues)
		}
		if cues[0].Start != 0 || cues[0].End <= 0 || cues[1].Start != cues[0].End || cues[1].End <= cues[1].Start {
			t.Fatalf("%s: the second sample must start where the first ends: %+v", id, cues)
		}
	}
}
