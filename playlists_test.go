package goshl

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Eyevinn/hls-m3u8/m3u8"

	"github.com/eleven-am/goshl/internal/domain"
)

type renditionPathGen struct{ stubPathGen }

func (renditionPathGen) VariantPlaylist(sourceURL string, rendition string, streamType domain.StreamType) string {
	return "/" + string(streamType) + "/" + rendition + ".m3u8"
}

func playlistController(t *testing.T, meta *domain.Metadata) *Controller {
	t.Helper()
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	return NewController(Options{
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
	out, err := svc.VariantPlaylist(context.Background(), "file:///media", StreamVideo, "1080p")
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
