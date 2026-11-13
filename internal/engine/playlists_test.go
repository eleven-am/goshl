package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Eyevinn/hls-m3u8/m3u8"
	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/playlist"
	"github.com/eleven-am/goshl/internal/rendition"
)

func TestMasterPlaylistUsesCachedMetadata(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := &domain.Metadata{Video: domain.VideoStream{Width: 1920, Height: 1080, Bitrate: 5_000_000}, Audios: []domain.AudioStream{{Codec: "ac3", Channels: 6, Bitrate: 640_000}}}
	metaBytes, _ := json.Marshal(meta)
	svc := mustNew(Config{
		Storage:     &stubStorage{metaData: metaBytes, metaExists: true},
		Coordinator: &stubCoordinator{},
		PathGen:     stubPathGen{},
	})

	out, err := svc.MasterPlaylist(context.Background(), "file:///media")
	if err != nil {
		t.Fatalf("master playlist err: %v", err)
	}
	if out == "" || !strings.Contains(out, "#EXT-X-STREAM-INF") {
		t.Fatalf("expected playlist content, got %q", out)
	}
}

func masterWithSubtitles(t *testing.T, omit bool) string {
	t.Helper()
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := &domain.Metadata{
		Video:  domain.VideoStream{Width: 1920, Height: 1080, Bitrate: 5_000_000},
		Audios: []domain.AudioStream{{Codec: "aac", Channels: 2, Bitrate: 128_000, Language: "eng"}},
		Subtitles: []domain.SubtitleStream{
			{Index: 2, ID: "t2", Codec: "subrip", Language: "eng", Default: true},
			{Index: 3, ID: "t3", Codec: "subrip", Language: "fra"},
		},
	}
	metaBytes, _ := json.Marshal(meta)
	svc := mustNew(Config{
		Storage:                &stubStorage{metaData: metaBytes, metaExists: true},
		Coordinator:            &stubCoordinator{},
		PathGen:                stubPathGen{},
		OmitSubtitleRenditions: omit,
	})

	out, err := svc.MasterPlaylist(context.Background(), "file:///media")
	if err != nil {
		t.Fatalf("master playlist err: %v", err)
	}
	return out
}

func TestMasterOmitsSubtitleRenditionsWhenConfigured(t *testing.T) {
	out := masterWithSubtitles(t, true)
	if !strings.Contains(out, "#EXT-X-STREAM-INF") {
		t.Fatalf("expected variants in playlist: %s", out)
	}
	for _, unwanted := range []string{"TYPE=SUBTITLES", "SUBTITLES="} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("playlist must not contain %q: %s", unwanted, out)
		}
	}
}

func TestMasterAdvertisesSubtitleRenditionsByDefault(t *testing.T) {
	out := masterWithSubtitles(t, false)
	if got := strings.Count(out, "TYPE=SUBTITLES"); got != 2 {
		t.Fatalf("expected 2 subtitle renditions, got %d: %s", got, out)
	}
	if !strings.Contains(out, "SUBTITLES=\"subtitles\"") {
		t.Fatalf("expected variants to reference the subtitle group: %s", out)
	}
}

func TestVariantPlaylistReturnsPlaylist(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := testVideoMetadata(12, []float64{0, 6, 12})
	metaBytes, _ := json.Marshal(meta)
	svc := mustNew(Config{
		Storage:     &stubStorage{metaData: metaBytes, metaExists: true},
		Coordinator: &stubCoordinator{},
		PathGen:     stubPathGen{},
	})

	out, err := svc.VariantPlaylist(context.Background(), "file:///media", domain.StreamVideo, "1080p")
	if err != nil {
		t.Fatalf("variant playlist err: %v", err)
	}
	if !strings.Contains(out, "#EXTM3U") || !strings.Contains(out, "#EXTINF") {
		t.Fatalf("expected valid playlist, got %q", out)
	}
}

type renditionPathGen struct{ stubPathGen }

func (renditionPathGen) VariantPlaylist(sourceURL string, rendition string, streamType domain.StreamType) string {
	return "/" + string(streamType) + "/" + rendition + ".m3u8"
}

func playlistController(t *testing.T, meta *domain.Metadata) *Engine {
	t.Helper()
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	return mustNew(Config{
		Storage:     &stubStorage{metaData: metaBytes, metaExists: true},
		Coordinator: &stubCoordinator{},
		PathGen:     renditionPathGen{},
	})
}

func decodeMasterPlaylist(t *testing.T, out string) *m3u8.MasterPlaylist {
	t.Helper()
	decoded, listType, err := m3u8.DecodeFrom(strings.NewReader(out), true)
	if err != nil || listType != m3u8.MASTER {
		t.Fatalf("decode master (list type %v): %v\n%s", listType, err, out)
	}
	return decoded.(*m3u8.MasterPlaylist)
}

func audioGroupMembers(master *m3u8.MasterPlaylist) map[string]map[string]*m3u8.Alternative {
	groups := map[string]map[string]*m3u8.Alternative{}
	for _, variant := range master.Variants {
		for _, alternative := range variant.Alternatives {
			if alternative.Type != "AUDIO" {
				continue
			}
			if groups[alternative.GroupId] == nil {
				groups[alternative.GroupId] = map[string]*m3u8.Alternative{}
			}
			groups[alternative.GroupId][alternative.URI] = alternative
		}
	}
	return groups
}

func TestMasterPlaylistIncludesAllAudioTracks(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	svc := playlistController(t, &domain.Metadata{
		Video: domain.VideoStream{Width: 1920, Height: 1080, Bitrate: 5_000_000},
		Audios: []domain.AudioStream{
			{Codec: "aac", Channels: 2, Bitrate: 128_000, Language: "eng"},
			{Codec: "ac3", Channels: 6, Bitrate: 640_000, Language: "spa", Default: true},
		},
	})

	out, err := svc.MasterPlaylist(context.Background(), "file:///media")
	if err != nil {
		t.Fatalf("master playlist err: %v", err)
	}
	groups := audioGroupMembers(decodeMasterPlaylist(t, out))

	want := map[string][]string{
		"audio-mp4a-40-2-2": {"/audio/audio0_aac_stereo.m3u8", "/audio/audio1_aac_stereo.m3u8"},
		"audio-mp4a-40-2-6": {"/audio/audio0_aac_stereo.m3u8", "/audio/audio1_aac_surround.m3u8"},
		"audio-ac-3-6":      {"/audio/audio0_aac_stereo.m3u8", "/audio/audio1_ac3_passthrough.m3u8"},
	}
	if len(groups) != len(want) {
		t.Fatalf("expected %d audio groups, got %d\n%s", len(want), len(groups), out)
	}
	for id, uris := range want {
		members := groups[id]
		if len(members) != len(uris) {
			t.Fatalf("group %q has %d members, want %d\n%s", id, len(members), len(uris), out)
		}
		defaults := 0
		for _, uri := range uris {
			member := members[uri]
			if member == nil {
				t.Fatalf("group %q is missing %q\n%s", id, uri, out)
			}
			if member.Default {
				defaults++
			}
		}
		if defaults != 1 {
			t.Fatalf("group %q has %d DEFAULT=YES members, want 1\n%s", id, defaults, out)
		}
	}
}

func TestAudioOnlyMasterPlaylistHasPlayableVariants(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	svc := playlistController(t, &domain.Metadata{
		Duration:  12,
		Audios:    []domain.AudioStream{{Codec: "ac3", Channels: 6, Language: "eng"}},
		Subtitles: []domain.SubtitleStream{{Index: 1, ID: "subtitle-1", Codec: "subrip", Language: "eng"}},
	})

	out, err := svc.MasterPlaylist(context.Background(), "file:///media")
	if err != nil {
		t.Fatalf("master playlist err: %v", err)
	}
	master := decodeMasterPlaylist(t, out)
	if len(master.Variants) != 3 {
		t.Fatalf("expected one variant per audio group, got %d\n%s", len(master.Variants), out)
	}
	for _, variant := range master.Variants {
		if variant.Bandwidth == 0 || variant.Codecs == "" || variant.Audio == "" || variant.Subtitles != "subtitles" {
			t.Fatalf("audio-only variant %q is incomplete: %+v\n%s", variant.URI, variant.VariantParams, out)
		}
	}
}

func TestMasterPlaylistRejectsSourceWithoutStreams(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	svc := playlistController(t, &domain.Metadata{Duration: 12})
	out, err := svc.MasterPlaylist(context.Background(), "file:///media")
	if err == nil || out != "" {
		t.Fatalf("expected an error and no playlist for a source without streams, got %q, %v", out, err)
	}
}

func TestVariantPlaylistRejectsEmptyPlan(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	svc := playlistController(t, &domain.Metadata{
		Video:  domain.VideoStream{Width: 1920, Height: 1080, Bitrate: 5_000_000},
		Audios: []domain.AudioStream{{Codec: "aac", Channels: 2, Bitrate: 128_000}},
	})
	out, err := svc.VariantPlaylist(context.Background(), "file:///media", domain.StreamVideo, "1080p")
	if !errors.Is(err, ErrSegmentOutOfRange) || out != "" {
		t.Fatalf("expected ErrSegmentOutOfRange and no playlist, got %q, %v", out, err)
	}
}

func TestSubtitlePlaylistRejectsZeroDuration(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	svc := playlistController(t, &domain.Metadata{
		Video:     domain.VideoStream{Width: 1920, Height: 1080, Bitrate: 5_000_000},
		Subtitles: []domain.SubtitleStream{{Index: 1, ID: "subtitle-1", Codec: "subrip", Language: "eng"}},
	})
	out, err := svc.SubtitlePlaylist(context.Background(), "file:///media", "subtitle-1")
	if err == nil || errors.Is(err, ErrSegmentOutOfRange) || out != "" {
		t.Fatalf("expected a plain error and no playlist, got %q, %v", out, err)
	}
}

type jobCapturingCoordinator struct {
	*segTestCoordinator
	jobs chan domain.Job
}

func (c *jobCapturingCoordinator) Enqueue(ctx context.Context, job domain.Job) error {
	c.jobs <- job
	return c.segTestCoordinator.Enqueue(ctx, job)
}

func renditionTestController(t *testing.T, meta *domain.Metadata, target float64) (*Engine, *jobCapturingCoordinator) {
	t.Helper()
	raw, err := jsonMetadata(meta)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &jobCapturingCoordinator{segTestCoordinator: newSegTestCoordinator(), jobs: make(chan domain.Job, 4)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	controller, err := New(ctx, Config{
		Storage:        &segTestStorage{meta: raw},
		Coordinator:    coordinator,
		PathGen:        stubPathGen{},
		TargetDuration: target,
		SegmentTimeout: 200 * time.Millisecond,
		JobTimeout:     5 * time.Second,
		SourceRevision: func(context.Context, string) (string, error) { return "r1", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return controller, coordinator
}

func capturedJob(t *testing.T, controller *Engine, coordinator *jobCapturingCoordinator, name string, index int) domain.Job {
	t.Helper()
	if _, err := controller.Segment(context.Background(), "/x.mp4", domain.StreamVideo, name, index); !errors.Is(err, ErrSegmentTimeout) {
		t.Fatalf("segment %d: error = %v, want a timeout after the job is enqueued", index, err)
	}
	select {
	case job := <-coordinator.jobs:
		return job
	default:
		t.Fatalf("segment %d enqueued no job", index)
		return domain.Job{}
	}
}

func TestSplitPlanSourceRungIsTranscodedWithoutCopyNeighbours(t *testing.T) {
	meta := testVideoMetadata(32, []float64{0, 20, 26})
	meta.Video.Level = 41

	controller, coordinator := renditionTestController(t, meta, 6)
	if playlist.KeyframeAligned(meta.Keyframes, meta.Duration, 6) {
		t.Fatalf("fixture must need a split plan at a 6 s target")
	}
	job := capturedJob(t, controller, coordinator, "1080p", 2)
	if job.VideoRendition == nil || job.VideoRendition.Method != domain.Transcode || job.Trailing != nil || job.Overlap != nil {
		t.Fatalf("a split plan must transcode the source rung with no copy neighbours, got rendition %+v overlap %+v trailing %+v", job.VideoRendition, job.Overlap, job.Trailing)
	}
	if job.VideoRendition.Level != 40 || job.VideoRendition.BufferSize == 0 || job.VideoRendition.PeakBitrate == 0 {
		t.Fatalf("the job must carry the encoder facts, got %+v", job.VideoRendition)
	}
	master, err := controller.MasterPlaylist(context.Background(), "/x.mp4")
	if err != nil {
		t.Fatalf("master playlist: %v", err)
	}
	if !strings.Contains(master, "RESOLUTION=1920x1080,CODECS=\"avc1.640028\"") {
		t.Fatalf("master must advertise the transcoded source rung: %s", master)
	}

	aligned, alignedCoordinator := renditionTestController(t, meta, 12)
	if !playlist.KeyframeAligned(meta.Keyframes, meta.Duration, 12) {
		t.Fatalf("fixture must be keyframe aligned at a 12 s target")
	}
	job = capturedJob(t, aligned, alignedCoordinator, "1080p", 1)
	if job.VideoRendition == nil || job.VideoRendition.Method != domain.DirectStream || job.Overlap == nil {
		t.Fatalf("the same source at a 12 s target must copy the source rung, got rendition %+v overlap %+v", job.VideoRendition, job.Overlap)
	}
	master, err = aligned.MasterPlaylist(context.Background(), "/x.mp4")
	if err != nil {
		t.Fatalf("master playlist: %v", err)
	}
	if !strings.Contains(master, "RESOLUTION=1920x1080,CODECS=\"avc1.640029\"") {
		t.Fatalf("master must advertise the copied source rung at the source level: %s", master)
	}
}

func TestSourceWithoutEncodableVideoFailsWithReason(t *testing.T) {
	meta := testVideoMetadata(24, []float64{0, 6, 12, 18})
	meta.Video.Codec = "hevc"
	meta.Video.FrameRate = 5000
	controller, coordinator := renditionTestController(t, meta, 6)

	_, err := controller.MasterPlaylist(context.Background(), "/x.mp4")
	if !errors.Is(err, rendition.ErrNoEncodableVideo) || !strings.Contains(err.Error(), "360p") {
		t.Fatalf("master error = %v, want the no-encodable-rendition error naming the rejected rungs", err)
	}
	if _, err := controller.VariantPlaylist(context.Background(), "/x.mp4", domain.StreamVideo, "1080p"); !errors.Is(err, ErrRenditionNotFound) {
		t.Fatalf("variant error = %v, want ErrRenditionNotFound", err)
	}
	if _, err := controller.Segment(context.Background(), "/x.mp4", domain.StreamVideo, "1080p", 0); !errors.Is(err, ErrRenditionNotFound) || !strings.Contains(err.Error(), "5.2") {
		t.Fatalf("segment error = %v, want ErrRenditionNotFound with the level reason", err)
	}
	if len(coordinator.jobs) != 0 {
		t.Fatalf("no job may be enqueued for a source without an encodable rendition")
	}
}

func droppedRenditionEvents(t *testing.T, meta *domain.Metadata) ([]domain.Event, string, error) {
	t.Helper()
	raw, err := jsonMetadata(meta)
	if err != nil {
		t.Fatal(err)
	}
	var events []domain.Event
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	controller, err := New(ctx, Config{
		Storage:        &segTestStorage{meta: raw},
		Coordinator:    newSegTestCoordinator(),
		PathGen:        stubPathGen{},
		SourceRevision: func(context.Context, string) (string, error) { return "r1", nil },
		Observer: domain.ObserverFunc(func(_ context.Context, event domain.Event) {
			if event.Type == domain.EventRenditionDropped {
				events = append(events, event)
			}
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	master, err := controller.MasterPlaylist(context.Background(), "/x.mkv")
	return events, master, err
}

func TestMasterPlaylistReportsEveryDroppedRendition(t *testing.T) {
	meta := testVideoMetadata(24, []float64{0, 6, 12, 18})
	meta.Video.Codec = "hevc"

	events, _, err := droppedRenditionEvents(t, meta)
	if err != nil || len(events) != 0 {
		t.Fatalf("a 1080p source drops nothing, got %v and %+v", err, events)
	}

	meta.Video.Width, meta.Video.Height, meta.Video.Bitrate = 7680, 4320, 80_000_000
	events, master, err := droppedRenditionEvents(t, meta)
	if err != nil {
		t.Fatalf("master playlist: %v", err)
	}
	if len(events) != 1 || events[0].Rendition != "4320p" || events[0].StreamType != domain.StreamVideo || events[0].MediaKey == "" || !strings.Contains(events[0].Error, "5.2") || events[0].Time.IsZero() {
		t.Fatalf("expected one dropped-rendition event for 4320p with its reason, got %+v", events)
	}
	if strings.Contains(master, "7680x4320") || !strings.Contains(master, "RESOLUTION=3840x2160") {
		t.Fatalf("master must offer 2160p and not the dropped rung: %s", master)
	}

	meta.Video.Width, meta.Video.Height, meta.Video.FrameRate = 1920, 1080, 5000
	events, _, err = droppedRenditionEvents(t, meta)
	if !errors.Is(err, rendition.ErrNoEncodableVideo) {
		t.Fatalf("master error = %v, want the no-encodable-rendition error", err)
	}
	var names []string
	for _, event := range events {
		names = append(names, event.Rendition)
	}
	if strings.Join(names, ",") != "1080p,720p,480p,360p" {
		t.Fatalf("every dropped rung must be reported before the failure, got %v", names)
	}
}
