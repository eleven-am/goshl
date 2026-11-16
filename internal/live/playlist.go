package live

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
)

const (
	RenditionName = "live"
	graceSegments = 4
)

type entry struct {
	sequence        int
	file            string
	duration        float64
	discontinuity   bool
	discontinuities int
	started         time.Time
}

type window struct {
	entries  []entry
	size     int
	target   int
	timeline float64
}

func (w *window) add(file string, sequence int, duration float64, discontinuity bool, now time.Time) []entry {
	item := entry{sequence: sequence, file: file, duration: duration}
	if len(w.entries) > 0 {
		last := w.entries[len(w.entries)-1]
		item.discontinuities = last.discontinuities
		item.started = last.started.Add(time.Duration(last.duration * float64(time.Second)))
		item.discontinuity = discontinuity
	}
	if arrived := now.Add(-time.Duration(duration * float64(time.Second))); len(w.entries) == 0 || item.discontinuity && arrived.After(item.started) {
		item.started = arrived
	}
	if item.discontinuity {
		item.discontinuities++
	}
	w.entries = append(w.entries, item)
	w.timeline += duration
	w.target = max(w.target, int(math.Round(duration)), 1)
	keep := w.size + graceSegments
	if len(w.entries) <= keep {
		return nil
	}
	evicted := append([]entry(nil), w.entries[:len(w.entries)-keep]...)
	w.entries = append([]entry(nil), w.entries[len(w.entries)-keep:]...)
	return evicted
}

func (w *window) visible() []entry {
	if len(w.entries) <= w.size {
		return w.entries
	}
	return w.entries[len(w.entries)-w.size:]
}

func (w *window) find(sequence int) (entry, bool) {
	for _, item := range w.entries {
		if item.sequence == sequence {
			return item, true
		}
	}
	return entry{}, false
}

func (w *window) render(pathGen domain.PathGenerator, key string) string {
	visible := w.visible()
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:4\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", w.target)
	if len(visible) == 0 {
		b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
		return b.String()
	}
	first := visible[0]
	sequence := first.discontinuities
	if first.discontinuity {
		sequence--
	}
	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", first.sequence)
	fmt.Fprintf(&b, "#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", sequence)
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	for _, item := range visible {
		if item.discontinuity {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n", item.started.UTC().Format("2006-01-02T15:04:05.000Z"))
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n", item.duration)
		b.WriteString(pathGen.Segment(key, RenditionName, domain.StreamVideo, item.sequence) + "\n")
	}
	return b.String()
}
