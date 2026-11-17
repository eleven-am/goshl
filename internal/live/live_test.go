package live_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/encoderslots"
	"github.com/eleven-am/goshl/internal/ffmpeg"
	"github.com/eleven-am/goshl/internal/hwaccel"
	"github.com/eleven-am/goshl/internal/live"
	"github.com/eleven-am/goshl/internal/live/livetest"
	"github.com/eleven-am/goshl/internal/testmedia"
)

type testPaths struct{}

func (testPaths) VariantPlaylist(string, string, domain.StreamType) string { return "" }
func (testPaths) Segment(key string, rendition string, _ domain.StreamType, index int) string {
	return fmt.Sprintf("/%s/%s/%d.ts", key, rendition, index)
}
func (testPaths) Sprite(string, int) string              { return "" }
func (testPaths) SubtitlePlaylist(string, string) string { return "" }
func (testPaths) SubtitleVTT(string, string) string      { return "" }

type observed struct {
	kind, label, class string
}

type recorder struct {
	mu     sync.Mutex
	events []observed
}

func (r *recorder) observe(kind, label, class string) {
	r.mu.Lock()
	r.events = append(r.events, observed{kind: kind, label: label, class: class})
	r.mu.Unlock()
}

func (r *recorder) types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var names []string
	for _, event := range r.events {
		names = append(names, event.kind)
	}
	return names
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	return testmedia.Live(t, testmedia.LiveKind(name))
}

type rig struct {
	manager  *live.Manager
	recorder *recorder
	root     string
	slots    *encoderslots.Budget
}

func newRig(t *testing.T, mutate func(*live.Config)) *rig {
	t.Helper()
	root := t.TempDir()
	rec := &recorder{}
	config := live.Config{
		PathGen: testPaths{},
		Builder: ffmpeg.NewCommandBuilder(hwaccel.NewConfig(domain.AccelNone)),
		Observe: rec.observe,
		Dir:     root,
	}
	if mutate != nil {
		mutate(&config)
	}
	manager := live.NewManager(config)
	t.Cleanup(manager.Stop)
	return &rig{manager: manager, recorder: rec, root: root, slots: config.Slots}
}

func fastOptions() live.Options {
	return live.Options{SegmentDuration: 2 * time.Second, WindowSegments: 4, IdleTimeout: 30 * time.Second}
}

func source(t *testing.T, name string) (live.Source, *livetest.Control) {
	src, control := livetest.FileSource(fixture(t, name), name)
	control.Burst = 5 * time.Second
	return src, control
}

func ready(t *testing.T, session *live.Session) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if err := session.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
}

type parsedPlaylist struct {
	text            string
	target          int
	mediaSequence   int
	discontinuitySq int
	sequences       []int
	durations       []float64
	discontinuities map[int]bool
	dates           int
}

var uriPattern = regexp.MustCompile(`/(\d+)\.ts$`)

func playlist(t *testing.T, session *live.Session) parsedPlaylist {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	text, err := session.Playlist(ctx)
	if err != nil {
		t.Fatalf("Playlist: %v", err)
	}
	parsed := parsedPlaylist{text: text, discontinuities: map[int]bool{}, mediaSequence: -1}
	pending := false
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
			parsed.target, _ = strconv.Atoi(strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:"))
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			parsed.mediaSequence, _ = strconv.Atoi(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"))
		case strings.HasPrefix(line, "#EXT-X-DISCONTINUITY-SEQUENCE:"):
			parsed.discontinuitySq, _ = strconv.Atoi(strings.TrimPrefix(line, "#EXT-X-DISCONTINUITY-SEQUENCE:"))
		case line == "#EXT-X-DISCONTINUITY":
			pending = true
		case strings.HasPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:"):
			parsed.dates++
		case strings.HasPrefix(line, "#EXTINF:"):
			value, _ := strconv.ParseFloat(strings.TrimSuffix(strings.TrimPrefix(line, "#EXTINF:"), ","), 64)
			parsed.durations = append(parsed.durations, value)
		case strings.HasPrefix(line, "#EXT-X-ENDLIST"):
			t.Fatalf("live playlist has ENDLIST")
		case line != "" && !strings.HasPrefix(line, "#"):
			match := uriPattern.FindStringSubmatch(line)
			if match == nil {
				t.Fatalf("unexpected uri %q", line)
			}
			sequence, _ := strconv.Atoi(match[1])
			parsed.sequences = append(parsed.sequences, sequence)
			parsed.discontinuities[sequence] = pending
			pending = false
		}
	}
	if len(parsed.sequences) == 0 || parsed.sequences[0] != parsed.mediaSequence {
		t.Fatalf("media sequence does not match first uri:\n%s", text)
	}
	for index := 1; index < len(parsed.sequences); index++ {
		if parsed.sequences[index] != parsed.sequences[index-1]+1 {
			t.Fatalf("sequence gap:\n%s", text)
		}
	}
	if parsed.dates != len(parsed.sequences) {
		t.Fatalf("missing program date time:\n%s", text)
	}
	return parsed
}

func waitFor(t *testing.T, limit time.Duration, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type segmentFacts struct {
	video, audio             string
	width, height            int
	fieldOrder               string
	channels                 int
	primaries                string
	firstVideo, lastVideo    float64
	firstAudio, lastAudio    float64
	startsWithKeyframe       bool
	videoPackets, audioCount int
}

func inspect(t *testing.T, session *live.Session, sequence int) segmentFacts {
	t.Helper()
	data, err := session.Segment(context.Background(), strconv.Itoa(sequence))
	if err != nil {
		t.Fatalf("Segment %d: %v", sequence, err)
	}
	path := filepath.Join(t.TempDir(), "segment.ts")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("ffprobe", "-v", "error", "-show_streams", "-show_packets", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	var parsed struct {
		Streams []struct {
			Index      int    `json:"index"`
			CodecType  string `json:"codec_type"`
			CodecName  string `json:"codec_name"`
			Width      int    `json:"width"`
			Height     int    `json:"height"`
			FieldOrder string `json:"field_order"`
			Channels   int    `json:"channels"`
			Primaries  string `json:"color_primaries"`
		} `json:"streams"`
		Packets []struct {
			StreamIndex int    `json:"stream_index"`
			PTS         string `json:"pts_time"`
			Flags       string `json:"flags"`
		} `json:"packets"`
	}
	if err := json.Unmarshal(output, &parsed); err != nil {
		t.Fatal(err)
	}
	facts := segmentFacts{firstVideo: -1, firstAudio: -1}
	kinds := map[int]string{}
	for _, stream := range parsed.Streams {
		kinds[stream.Index] = stream.CodecType
		if stream.CodecType == "video" {
			facts.video, facts.width, facts.height, facts.fieldOrder, facts.primaries = stream.CodecName, stream.Width, stream.Height, stream.FieldOrder, stream.Primaries
		}
		if stream.CodecType == "audio" {
			facts.audio, facts.channels = stream.CodecName, stream.Channels
		}
	}
	for _, packet := range parsed.Packets {
		pts, err := strconv.ParseFloat(packet.PTS, 64)
		if err != nil {
			continue
		}
		switch kinds[packet.StreamIndex] {
		case "video":
			if facts.videoPackets == 0 {
				facts.startsWithKeyframe = strings.HasPrefix(packet.Flags, "K")
			}
			facts.videoPackets++
			if facts.firstVideo < 0 || pts < facts.firstVideo {
				facts.firstVideo = pts
			}
			facts.lastVideo = max(facts.lastVideo, pts)
		case "audio":
			facts.audioCount++
			if facts.firstAudio < 0 || pts < facts.firstAudio {
				facts.firstAudio = pts
			}
			facts.lastAudio = max(facts.lastAudio, pts)
		}
	}
	return facts
}

func ffmpegProcesses(root string) int {
	output, _ := exec.Command("pgrep", "-f", "ffmpeg.*"+regexp.QuoteMeta(root)).Output()
	return len(strings.Fields(string(output)))
}

func TestCodecMatrix(t *testing.T) {
	cases := []struct {
		fixture      string
		video, audio live.Action
		interlaced   bool
		height       int
		width        int
	}{
		{"h264_aac", live.ActionCopy, live.ActionCopy, false, 720, 1280},
		{"h264_mp2", live.ActionCopy, live.ActionTranscode, false, 720, 1280},
		{"mpeg2i_mp2", live.ActionTranscode, live.ActionTranscode, true, 576, 1024},
		{"hevc_ac3", live.ActionTranscode, live.ActionTranscode, false, 720, 1280},
	}
	for _, test := range cases {
		t.Run(test.fixture, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, nil)
			src, _ := source(t, test.fixture)
			session, err := r.manager.Live(context.Background(), test.fixture, src, fastOptions())
			if err != nil {
				t.Fatal(err)
			}
			ready(t, session)
			info := session.Info()
			if info.VideoAction != test.video || info.AudioAction != test.audio || info.Video.Interlaced != test.interlaced {
				t.Fatalf("plan = %+v", info)
			}
			list := playlist(t, session)
			if len(list.sequences) < 3 {
				t.Fatalf("playlist has %d segments", len(list.sequences))
			}
			facts := inspect(t, session, list.sequences[0])
			if facts.video != "h264" || facts.audio != "aac" || facts.height != test.height || facts.width != test.width || !facts.startsWithKeyframe {
				t.Fatalf("segment facts = %+v", facts)
			}
			if test.video == live.ActionTranscode {
				if facts.fieldOrder != "progressive" || facts.channels != 2 || facts.primaries != "bt709" {
					t.Fatalf("transcoded facts = %+v", facts)
				}
			}
			for _, duration := range list.durations {
				if int(duration+0.5) > list.target {
					t.Fatalf("duration %v exceeds target %d", duration, list.target)
				}
				if duration < 1.9 || duration > 4.1 {
					t.Fatalf("duration %v is not near the 2 s segment duration:\n%s", duration, list.text)
				}
			}
			frame := 1 / info.Video.FrameRate
			if span := facts.lastVideo - facts.firstVideo + frame; span < list.durations[0]-0.1 || span > list.durations[0]+0.1 {
				t.Fatalf("EXTINF %v but the segment holds %v s of video", list.durations[0], span)
			}
			if state := session.State(); state.Status != live.StatusRunning {
				t.Fatalf("state = %+v", state)
			}
		})
	}
}

func TestConcurrentLiveSharesOneSession(t *testing.T) {
	r := newRig(t, nil)
	src, control := source(t, "h264_aac")
	var wg sync.WaitGroup
	sessions := make([]*live.Session, 24)
	for index := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			session, err := r.manager.Live(context.Background(), "shared", src, fastOptions())
			if err != nil {
				t.Error(err)
				return
			}
			sessions[index] = session
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()
			if _, err := session.Playlist(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	for _, session := range sessions {
		if session != sessions[0] {
			t.Fatal("Live returned different sessions for one key")
		}
	}
	if control.Opens() != 1 {
		t.Fatalf("source opened %d times", control.Opens())
	}
	if count := ffmpegProcesses(r.root); count != 1 {
		t.Fatalf("%d ffmpeg processes for one key", count)
	}
	if r.manager.Count() != 1 {
		t.Fatalf("manager holds %d sessions", r.manager.Count())
	}
}

func TestSwitchSourceMarksDiscontinuityAndKeepsTimeline(t *testing.T) {
	r := newRig(t, nil)
	first, _ := source(t, "h264_aac")
	second, _ := source(t, "mpeg2i_mp2")
	options := fastOptions()
	options.WindowSegments = 8
	session, err := r.manager.Live(context.Background(), "switch", first, options)
	if err != nil {
		t.Fatal(err)
	}
	ready(t, session)
	before := playlist(t, session)
	most := 0
	watching := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		for {
			most = max(most, ffmpegProcesses(r.root))
			select {
			case <-watching:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	if err := session.SwitchSource(context.Background(), second); err != nil {
		t.Fatalf("SwitchSource: %v", err)
	}
	var after parsedPlaylist
	var boundary int
	defer func() {
		close(watching)
		<-watched
		if most != 1 {
			t.Errorf("saw %d ffmpeg processes at once during the switch", most)
		}
	}()
	waitFor(t, 30*time.Second, "discontinuity plus two segments", func() bool {
		after = playlist(t, session)
		for _, sequence := range after.sequences {
			if after.discontinuities[sequence] {
				boundary = sequence
				return after.sequences[len(after.sequences)-1] >= boundary+1
			}
		}
		return false
	})
	if after.mediaSequence < before.mediaSequence || boundary <= before.sequences[len(before.sequences)-1]-0 && boundary <= before.sequences[0] {
		t.Fatalf("sequence went backwards:\n%s\n%s", before.text, after.text)
	}
	previous := inspect(t, session, boundary-1)
	next := inspect(t, session, boundary)
	if next.firstVideo <= previous.lastVideo || next.firstAudio <= previous.lastAudio {
		t.Fatalf("timestamps went backwards across the switch: %+v then %+v", previous, next)
	}
	if next.video != "h264" || next.audio != "aac" || next.height != 576 {
		t.Fatalf("post-switch facts = %+v", next)
	}
	if session.Label() != "mpeg2i_mp2" || session.Epochs() != 2 {
		t.Fatalf("label %q epochs %d", session.Label(), session.Epochs())
	}
	count := 0
	for _, flagged := range after.discontinuities {
		if flagged {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("%d discontinuities:\n%s", count, after.text)
	}
}

func TestFailedSwitchKeepsCurrentSource(t *testing.T) {
	r := newRig(t, nil)
	first, control := source(t, "h264_aac")
	session, err := r.manager.Live(context.Background(), "keep", first, fastOptions())
	if err != nil {
		t.Fatal(err)
	}
	ready(t, session)
	failure := errors.New("upstream said no")
	broken := live.Source{Label: "broken", Open: func(context.Context) (io.ReadCloser, error) { return nil, failure }}
	err = session.SwitchSource(context.Background(), broken)
	if !errors.Is(err, live.ErrSourceUnavailable) || !errors.Is(err, failure) {
		t.Fatalf("SwitchSource error = %v", err)
	}
	last := playlist(t, session).sequences
	waitFor(t, 20*time.Second, "old source keeps producing", func() bool {
		now := playlist(t, session).sequences
		return now[len(now)-1] > last[len(last)-1]
	})
	if session.Label() != "h264_aac" || control.Opens() != 1 || session.Epochs() != 1 {
		t.Fatalf("label %q opens %d epochs %d", session.Label(), control.Opens(), session.Epochs())
	}
}

func TestReaderErrorReconnectsWithDiscontinuity(t *testing.T) {
	r := newRig(t, nil)
	src, control := source(t, "h264_mp2")
	options := fastOptions()
	options.WindowSegments = 8
	session, err := r.manager.Live(context.Background(), "reconnect", src, options)
	if err != nil {
		t.Fatal(err)
	}
	ready(t, session)
	control.Fail()
	var list parsedPlaylist
	var boundary int
	waitFor(t, 30*time.Second, "segments after reconnect", func() bool {
		list = playlist(t, session)
		for _, sequence := range list.sequences {
			if list.discontinuities[sequence] {
				boundary = sequence
				return true
			}
		}
		return false
	})
	if control.Opens() != 2 || session.Epochs() != 2 {
		t.Fatalf("opens %d epochs %d", control.Opens(), session.Epochs())
	}
	previous := inspect(t, session, boundary-1)
	next := inspect(t, session, boundary)
	if next.firstVideo <= previous.lastVideo {
		t.Fatalf("timestamps went backwards across the reconnect: %+v then %+v", previous, next)
	}
	if state := session.State(); state.Status != live.StatusRunning {
		t.Fatalf("state = %+v", state)
	}
	if !strings.Contains(strings.Join(r.recorder.types(), ","), "live.reconnecting") {
		t.Fatalf("events = %v", r.recorder.types())
	}
}

func TestStalledReaderReconnects(t *testing.T) {
	r := newRig(t, func(config *live.Config) { config.StallTimeout = 2 * time.Second })
	src, control := source(t, "h264_aac")
	session, err := r.manager.Live(context.Background(), "stall", src, fastOptions())
	if err != nil {
		t.Fatal(err)
	}
	ready(t, session)
	control.Stall()
	waitFor(t, 30*time.Second, "reopen after stall", func() bool { return control.Opens() == 2 && session.Epochs() == 2 })
}

func TestRetriesAreBoundedThenFailed(t *testing.T) {
	r := newRig(t, func(config *live.Config) {
		config.ReconnectAttempts = 3
		config.ReconnectInitial = 50 * time.Millisecond
		config.ReconnectMax = 100 * time.Millisecond
	})
	src, control := source(t, "h264_aac")
	session, err := r.manager.Live(context.Background(), "bounded", src, fastOptions())
	if err != nil {
		t.Fatal(err)
	}
	ready(t, session)
	control.RefuseOpens(1000)
	control.Fail()
	select {
	case <-session.Done():
	case <-time.After(20 * time.Second):
		t.Fatal("session did not fail")
	}
	state := session.State()
	if state.Status != live.StatusFailed || !errors.Is(state.Err, live.ErrSourceUnavailable) || !errors.Is(state.Err, livetest.ErrInjected) {
		t.Fatalf("state = %+v", state)
	}
	if opens := control.Opens(); opens != 1+3 {
		t.Fatalf("opens = %d", opens)
	}
	if _, err := session.Playlist(context.Background()); !errors.Is(err, live.ErrSourceUnavailable) {
		t.Fatalf("Playlist after failure = %v", err)
	}
	if r.manager.Count() != 0 {
		t.Fatal("failed session still registered")
	}
	entries, _ := os.ReadDir(r.root)
	if len(entries) != 0 {
		t.Fatalf("files left after failure: %d", len(entries))
	}
}

func TestIdleSessionStopsAndDeletesFiles(t *testing.T) {
	r := newRig(t, nil)
	src, _ := source(t, "h264_aac")
	options := fastOptions()
	options.IdleTimeout = 3 * time.Second
	session, err := r.manager.Live(context.Background(), "idle", src, options)
	if err != nil {
		t.Fatal(err)
	}
	ready(t, session)
	for index := 0; index < 5; index++ {
		playlist(t, session)
		time.Sleep(time.Second)
	}
	if state := session.State(); state.Status != live.StatusRunning {
		t.Fatalf("session stopped while being read: %+v", state)
	}
	if ffmpegProcesses(r.root) != 1 {
		t.Fatal("expected one ffmpeg while active")
	}
	select {
	case <-session.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("idle session did not stop")
	}
	if state := session.State(); state.Status != live.StatusStopped {
		t.Fatalf("state = %+v", state)
	}
	waitFor(t, 5*time.Second, "ffmpeg exit", func() bool { return ffmpegProcesses(r.root) == 0 })
	entries, _ := os.ReadDir(r.root)
	if len(entries) != 0 {
		t.Fatalf("files left after idle stop: %d", len(entries))
	}
	if _, err := session.Playlist(context.Background()); !errors.Is(err, live.ErrStopped) {
		t.Fatalf("Playlist after stop = %v", err)
	}
	again, err := r.manager.Live(context.Background(), "idle", src, options)
	if err != nil || again == session {
		t.Fatalf("Live after idle stop: %v same=%v", err, again == session)
	}
}

func TestWindowSlidesAndDeletesOldSegments(t *testing.T) {
	r := newRig(t, nil)
	src, control := livetest.FileSource(fixture(t, "h264_aac"), "h264_aac")
	control.Burst = 30 * time.Second
	options := fastOptions()
	options.WindowSegments = 3
	session, err := r.manager.Live(context.Background(), "window", src, options)
	if err != nil {
		t.Fatal(err)
	}
	ready(t, session)
	var list parsedPlaylist
	waitFor(t, 30*time.Second, "window to slide", func() bool {
		list = playlist(t, session)
		return list.mediaSequence >= 6
	})
	if len(list.sequences) != 3 {
		t.Fatalf("window has %d segments", len(list.sequences))
	}
	if _, err := session.Segment(context.Background(), "0"); !errors.Is(err, live.ErrSegmentNotFound) {
		t.Fatalf("old segment still served: %v", err)
	}
	dirs, _ := os.ReadDir(r.root)
	files, _ := os.ReadDir(filepath.Join(r.root, dirs[0].Name()))
	if len(files) > 3+4+1 {
		t.Fatalf("%d files on disk", len(files))
	}
}

func TestErrorsAreClassified(t *testing.T) {
	r := newRig(t, func(config *live.Config) { config.ProbeBudget = 2 * time.Second })
	failure := errors.New("dial failed")
	cases := []struct {
		name string
		open func(context.Context) (io.ReadCloser, error)
		want error
	}{
		{"open error", func(context.Context) (io.ReadCloser, error) { return nil, failure }, live.ErrSourceUnavailable},
		{"empty", func(context.Context) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("")), nil }, live.ErrNoData},
		{"not ts", func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(strings.Repeat("<html>not a stream</html>", 4000))), nil
		}, live.ErrNoData},
		{"silent", func(context.Context) (io.ReadCloser, error) { return newSilent(), nil }, live.ErrNoData},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			session, err := r.manager.Live(context.Background(), test.name, live.Source{Label: test.name, Open: test.open}, fastOptions())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			err = session.WaitReady(ctx)
			if !errors.Is(err, test.want) {
				t.Fatalf("WaitReady = %v, want %v", err, test.want)
			}
			if test.name == "open error" && !errors.Is(err, failure) {
				t.Fatalf("reader error not preserved: %v", err)
			}
			if state := session.State(); state.Status != live.StatusFailed {
				t.Fatalf("state = %+v", state)
			}
			_, err = r.manager.Probe(context.Background(), live.Source{Label: test.name, Open: test.open}, 2*time.Second)
			if !errors.Is(err, test.want) {
				t.Fatalf("Probe = %v, want %v", err, test.want)
			}
		})
	}
}

type silent struct {
	closed chan struct{}
	once   sync.Once
}

func newSilent() *silent { return &silent{closed: make(chan struct{})} }

func (s *silent) Read([]byte) (int, error) {
	<-s.closed
	return 0, os.ErrClosed
}

func (s *silent) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

func TestUnsupportedCodec(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mpeg4.ts")
	if output, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25", "-f", "lavfi", "-i", "sine", "-t", "6", "-c:v", "mpeg4", "-c:a", "mp2", "-f", "mpegts", path).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, output)
	}
	r := newRig(t, nil)
	src, control := livetest.FileSource(path, "mpeg4")
	control.Unpaced = true
	session, err := r.manager.Live(context.Background(), "mpeg4", src, fastOptions())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := session.WaitReady(ctx); !errors.Is(err, live.ErrUnsupportedCodec) {
		t.Fatalf("WaitReady = %v", err)
	}
	if control.Opens() != 1 {
		t.Fatalf("unsupported codec was retried: %d opens", control.Opens())
	}
}

func TestCapacity(t *testing.T) {
	slots := encoderslots.New(3, 1)
	r := newRig(t, func(config *live.Config) {
		config.MaxSessions = 3
		config.Slots = slots
	})
	copyA, _ := source(t, "h264_aac")
	copyB, _ := source(t, "h264_mp2")
	transcodeA, _ := source(t, "hevc_ac3")
	transcodeB, _ := source(t, "mpeg2i_mp2")
	one, err := r.manager.Live(context.Background(), "copy-a", copyA, fastOptions())
	if err != nil {
		t.Fatal(err)
	}
	two, err := r.manager.Live(context.Background(), "transcode-a", transcodeA, fastOptions())
	if err != nil {
		t.Fatal(err)
	}
	ready(t, one)
	ready(t, two)
	three, err := r.manager.Live(context.Background(), "transcode-b", transcodeB, fastOptions())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := three.WaitReady(ctx); !errors.Is(err, live.ErrCapacity) {
		t.Fatalf("second transcoding session = %v", err)
	}
	if err := one.SwitchSource(context.Background(), transcodeB); !errors.Is(err, live.ErrCapacity) {
		t.Fatalf("switch to a transcoding source without a slot = %v", err)
	}
	four, err := r.manager.Live(context.Background(), "copy-b", copyB, fastOptions())
	if err != nil {
		t.Fatal(err)
	}
	ready(t, four)
	if _, err := r.manager.Live(context.Background(), "copy-c", copyA, fastOptions()); !errors.Is(err, live.ErrCapacity) {
		t.Fatalf("session over MaxSessions = %v", err)
	}
	vod := make(chan error, 2)
	for range 2 {
		go func() { vod <- slots.Acquire(context.Background()) }()
	}
	for range 2 {
		select {
		case err := <-vod:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("VOD could not take the slots live does not hold")
		}
	}
	blocked, cancelBlocked := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancelBlocked()
	if err := slots.Acquire(blocked); err == nil {
		t.Fatal("budget handed out more slots than its size")
	}
	two.Stop()
	if err := slots.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestManagerStopEndsEverything(t *testing.T) {
	r := newRig(t, nil)
	src, _ := source(t, "h264_aac")
	session, err := r.manager.Live(context.Background(), "stop", src, fastOptions())
	if err != nil {
		t.Fatal(err)
	}
	ready(t, session)
	r.manager.Stop()
	if state := session.State(); state.Status != live.StatusStopped {
		t.Fatalf("state = %+v", state)
	}
	if ffmpegProcesses(r.root) != 0 {
		t.Fatal("ffmpeg survived Stop")
	}
	entries, _ := os.ReadDir(r.root)
	if len(entries) != 0 {
		t.Fatalf("files left after Stop: %d", len(entries))
	}
	if _, err := r.manager.Live(context.Background(), "stop", src, fastOptions()); !errors.Is(err, live.ErrStopped) {
		t.Fatalf("Live after Stop = %v", err)
	}
}

func generated(t *testing.T, name string, args ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	full := append([]string{"-v", "error", "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=25", "-f", "lavfi", "-i", "sine=sample_rate=48000", "-t", "12"}, args...)
	if output, err := exec.Command("ffmpeg", append(full, "-f", "mpegts", path)...).CombinedOutput(); err != nil {
		t.Fatalf("fixture %s: %v %s", name, err, output)
	}
	return path
}

func TestAudioVariants(t *testing.T) {
	h264 := []string{"-c:v", "libx264", "-preset", "ultrafast", "-g", "25", "-pix_fmt", "yuv420p"}
	cases := []struct {
		name      string
		args      []string
		video     live.Action
		audio     live.Action
		wantAudio string
		selected  string
	}{
		{"video only", append(append([]string{}, h264...), "-an"), live.ActionCopy, live.ActionNone, "", ""},
		{"latm", append(append([]string{}, h264...), "-c:a", "aac", "-mpegts_flags", "+latm"), live.ActionCopy, live.ActionTranscode, "aac", ""},
		{"422 ten bit", []string{"-c:v", "libx264", "-preset", "ultrafast", "-g", "25", "-pix_fmt", "yuv422p10le", "-c:a", "aac"}, live.ActionTranscode, live.ActionCopy, "aac", ""},
		{"preferred language", append(append([]string{"-map", "0:v", "-map", "1:a", "-map", "1:a"}, h264...), "-c:a:0", "mp2", "-c:a:1", "ac3", "-metadata:s:a:0", "language=eng", "-metadata:s:a:1", "language=fra"), live.ActionCopy, live.ActionTranscode, "aac", "fra"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := generated(t, "variant.ts", test.args...)
			r := newRig(t, nil)
			src, control := livetest.FileSource(path, test.name)
			control.Burst = 8 * time.Second
			options := fastOptions()
			options.PreferredLanguage = "fra"
			session, err := r.manager.Live(context.Background(), "variant", src, options)
			if err != nil {
				t.Fatal(err)
			}
			ready(t, session)
			info := session.Info()
			if info.VideoAction != test.video || info.AudioAction != test.audio {
				t.Fatalf("plan = %+v", info)
			}
			if test.selected != "" && info.Audios[info.SelectedAudio].Language != test.selected {
				t.Fatalf("selected audio = %+v", info.Audios[info.SelectedAudio])
			}
			facts := inspect(t, session, playlist(t, session).sequences[0])
			if facts.video != "h264" || facts.audio != test.wantAudio || facts.videoPackets == 0 {
				t.Fatalf("segment facts = %+v", facts)
			}
			if test.wantAudio != "" && facts.audioCount == 0 {
				t.Fatalf("segment has no audio packets: %+v", facts)
			}
		})
	}
}

func TestProbeReportsCodecsAndResolutionWithinBudget(t *testing.T) {
	r := newRig(t, nil)
	src, control := livetest.FileSource(fixture(t, "mpeg2i_mp2"), "probe")
	control.Burst = 0
	started := time.Now()
	info, err := r.manager.Probe(context.Background(), src, 6*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 6*time.Second {
		t.Fatalf("probe took %v", elapsed)
	}
	if info.Video.Codec != "mpeg2video" || info.Video.Width != 720 || info.Video.Height != 576 || info.Video.DisplayW != 1024 || !info.Video.Interlaced {
		t.Fatalf("video = %+v", info.Video)
	}
	if len(info.Audios) != 1 || info.Audios[0].Codec != "mp2" || info.Audios[0].Language != "fra" || info.VideoAction != live.ActionTranscode || info.AudioAction != live.ActionTranscode {
		t.Fatalf("info = %+v", info)
	}
	if r.manager.Count() != 0 || ffmpegProcesses(r.root) != 0 {
		t.Fatal("probe left a session or a process behind")
	}
}

type secretReader struct {
	inner io.ReadCloser
	limit int
	read  int
}

func (s *secretReader) Read(buffer []byte) (int, error) {
	if s.read > s.limit {
		return 0, errSecret
	}
	n, err := s.inner.Read(buffer)
	s.read += n
	return n, err
}

func (s *secretReader) Close() error { return s.inner.Close() }

var errSecret = errors.New("Get \"http://secret-host.example/stream?token=SECRETTOKEN\": connection reset")

func TestTranscodeInterlacedDeinterlacesH264(t *testing.T) {
	cases := []struct {
		name   string
		option bool
		action live.Action
		field  string
	}{
		{"option on", true, live.ActionTranscode, "progressive"},
		{"option off", false, live.ActionCopy, "tt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, nil)
			src, _ := source(t, "h264i_aac")
			opts := fastOptions()
			opts.TranscodeInterlaced = tc.option
			session, err := r.manager.Live(context.Background(), "h264i", src, opts)
			if err != nil {
				t.Fatal(err)
			}
			ready(t, session)
			if info := session.Info(); info.VideoAction != tc.action || !info.Video.Interlaced {
				t.Fatalf("plan = %+v", info)
			}
			list := playlist(t, session)
			facts := inspect(t, session, list.sequences[0])
			if facts.video != "h264" || facts.fieldOrder != tc.field {
				t.Fatalf("segment facts = %+v", facts)
			}
		})
	}
}
