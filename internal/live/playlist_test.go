package live

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
)

type windowPaths struct{}

func (windowPaths) VariantPlaylist(string, string, domain.StreamType) string { return "" }
func (windowPaths) Segment(key string, rendition string, stream domain.StreamType, index int) string {
	return fmt.Sprintf("%s/%s/%s/%d", key, rendition, stream, index)
}
func (windowPaths) Sprite(string, int) string              { return "" }
func (windowPaths) SubtitlePlaylist(string, string) string { return "" }
func (windowPaths) SubtitleVTT(string, string) string      { return "" }

func TestWindowRendersSlidingLivePlaylist(t *testing.T) {
	w := window{size: 3, target: 4}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	steps := []struct {
		duration      float64
		discontinuity bool
		advance       time.Duration
	}{
		{4, false, 4 * time.Second},
		{4, false, 4 * time.Second},
		{1.5, false, 1500 * time.Millisecond},
		{4, true, time.Second},
		{5.6, false, 5600 * time.Millisecond},
		{4, true, 9 * time.Second},
		{4, false, 4 * time.Second},
		{4, false, 4 * time.Second},
		{4, false, 4 * time.Second},
		{4, false, 4 * time.Second},
	}
	wantDiscontinuitySequence := []int{0, 0, 0, 0, 0, 0, 1, 1, 2, 2}
	wantFirst := []int{0, 0, 0, 1, 2, 3, 4, 5, 6, 7}
	var previous time.Time
	var evicted []entry
	for index, step := range steps {
		now = now.Add(step.advance)
		evicted = append(evicted, w.add(fmt.Sprintf("seg-%d.ts", index), index, step.duration, step.discontinuity, now)...)
		text := w.render(windowPaths{}, "key")
		if !strings.Contains(text, fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d\n", wantFirst[index])) || !strings.Contains(text, fmt.Sprintf("#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", wantDiscontinuitySequence[index])) {
			t.Fatalf("step %d:\n%s", index, text)
		}
		if strings.Contains(text, "ENDLIST") || strings.Contains(text, "PLAYLIST-TYPE") {
			t.Fatalf("step %d is not a live playlist:\n%s", index, text)
		}
		last := w.entries[len(w.entries)-1]
		if last.started.Before(previous) {
			t.Fatalf("step %d: program date time went backwards", index)
		}
		previous = last.started.Add(time.Duration(last.duration * float64(time.Second)))
	}
	text := w.render(windowPaths{}, "key")
	want := "#EXTM3U\n#EXT-X-VERSION:4\n#EXT-X-TARGETDURATION:6\n#EXT-X-MEDIA-SEQUENCE:7\n#EXT-X-DISCONTINUITY-SEQUENCE:2\n#EXT-X-INDEPENDENT-SEGMENTS\n" +
		"#EXT-X-PROGRAM-DATE-TIME:2026-01-02T03:04:34.100Z\n#EXTINF:4.000,\nkey/live/video/7\n" +
		"#EXT-X-PROGRAM-DATE-TIME:2026-01-02T03:04:38.100Z\n#EXTINF:4.000,\nkey/live/video/8\n" +
		"#EXT-X-PROGRAM-DATE-TIME:2026-01-02T03:04:42.100Z\n#EXTINF:4.000,\nkey/live/video/9\n"
	if text != want {
		t.Fatalf("playlist:\n%s\nwant:\n%s", text, want)
	}
	if len(evicted) != 3 || evicted[0].sequence != 0 || evicted[2].sequence != 2 {
		t.Fatalf("evicted = %+v", evicted)
	}
	if _, ok := w.find(2); ok {
		t.Fatal("evicted segment still findable")
	}
	if _, ok := w.find(3); !ok {
		t.Fatal("grace segment not findable")
	}
	if w.timeline != 39.1 {
		t.Fatalf("timeline = %v", w.timeline)
	}
}

func TestWindowKeepsDiscontinuityTagOnFirstSegment(t *testing.T) {
	w := window{size: 2, target: 4}
	now := time.Unix(1000, 0)
	w.add("a", 0, 4, false, now)
	w.add("b", 1, 4, true, now.Add(4*time.Second))
	w.add("c", 2, 4, false, now.Add(8*time.Second))
	text := w.render(windowPaths{}, "key")
	if !strings.Contains(text, "#EXT-X-DISCONTINUITY-SEQUENCE:0\n#EXT-X-INDEPENDENT-SEGMENTS\n#EXT-X-DISCONTINUITY\n") {
		t.Fatalf("playlist:\n%s", text)
	}
	w.add("d", 3, 4, false, now.Add(12*time.Second))
	text = w.render(windowPaths{}, "key")
	if !strings.Contains(text, "#EXT-X-DISCONTINUITY-SEQUENCE:1\n") || strings.Contains(text, "#EXT-X-DISCONTINUITY\n") {
		t.Fatalf("playlist:\n%s", text)
	}
}

func TestWindowTargetDurationOnlyGrowsAndRounds(t *testing.T) {
	w := window{size: 3}
	now := time.Unix(1000, 0)
	cases := []struct {
		duration float64
		want     int
	}{
		{0.3, 1},
		{4.4, 4},
		{2, 4},
		{5.6, 6},
		{4, 6},
	}
	for index, c := range cases {
		w.add(fmt.Sprintf("s%d", index), index, c.duration, false, now.Add(time.Duration(index)*time.Second))
		if w.target != c.want {
			t.Fatalf("step %d: target = %d, want %d", index, w.target, c.want)
		}
		if !strings.Contains(w.render(windowPaths{}, "k"), fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", c.want)) {
			t.Fatalf("step %d: rendered target mismatch", index)
		}
	}
}

func TestWindowMediaSequenceIsMonotonicAcrossEpochs(t *testing.T) {
	w := window{size: 3, target: 4}
	now := time.Unix(2000, 0)
	previous := -1
	for index := 0; index < 12; index++ {
		w.add(fmt.Sprintf("s%d", index), index, 4, index%4 == 3, now.Add(time.Duration(index*4)*time.Second))
		text := w.render(windowPaths{}, "k")
		var got int
		for _, line := range strings.Split(text, "\n") {
			if rest, ok := strings.CutPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"); ok {
				fmt.Sscanf(rest, "%d", &got)
			}
		}
		if got < previous {
			t.Fatalf("step %d: media sequence went from %d to %d", index, previous, got)
		}
		previous = got
		if strings.Count(text, "#EXTINF") > 3 {
			t.Fatalf("step %d: more than the window size listed", index)
		}
	}
	if previous != 9 {
		t.Fatalf("final media sequence = %d", previous)
	}
}

func TestWindowDiscontinuityTagAtEveryEpochChange(t *testing.T) {
	w := window{size: 6, target: 4}
	now := time.Unix(3000, 0)
	flags := []bool{false, false, true, false, true, false}
	for index, flag := range flags {
		w.add(fmt.Sprintf("s%d", index), index, 4, flag, now.Add(time.Duration(index*4)*time.Second))
	}
	text := w.render(windowPaths{}, "k")
	if strings.Count(text, "#EXT-X-DISCONTINUITY\n") != 2 {
		t.Fatalf("playlist:\n%s", text)
	}
	if strings.Count(text, "#EXT-X-PROGRAM-DATE-TIME:") != 6 {
		t.Fatalf("playlist:\n%s", text)
	}
	if !strings.Contains(text, "#EXT-X-DISCONTINUITY\n#EXT-X-PROGRAM-DATE-TIME:2026") && !strings.Contains(text, "#EXT-X-DISCONTINUITY\n#EXT-X-PROGRAM-DATE-TIME:1970") {
		t.Fatalf("tag not directly before program date time:\n%s", text)
	}
}

func TestWindowEvictionReturnsFilesToDelete(t *testing.T) {
	w := window{size: 2, target: 4}
	now := time.Unix(4000, 0)
	var evicted []string
	for index := 0; index < 10; index++ {
		for _, item := range w.add(fmt.Sprintf("seg-%d.ts", index), index, 4, false, now.Add(time.Duration(index*4)*time.Second)) {
			evicted = append(evicted, item.file)
		}
	}
	want := []string{"seg-0.ts", "seg-1.ts", "seg-2.ts", "seg-3.ts"}
	if strings.Join(evicted, ",") != strings.Join(want, ",") {
		t.Fatalf("evicted = %v, want %v", evicted, want)
	}
	if len(w.entries) != 2+graceSegments {
		t.Fatalf("entries = %d", len(w.entries))
	}
}

func TestWindowEmptyRenderHasNoSegments(t *testing.T) {
	w := window{size: 3, target: 4}
	text := w.render(windowPaths{}, "k")
	if strings.Contains(text, "#EXTINF") || strings.Contains(text, "ENDLIST") {
		t.Fatalf("playlist:\n%s", text)
	}
}
