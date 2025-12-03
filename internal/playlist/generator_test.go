package playlist

import (
	"strconv"
	"strings"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
)

type staticPathGen struct{}

func (staticPathGen) MasterPlaylist(mediaID string) string {
	return "/" + mediaID + "/master.m3u8"
}

func (staticPathGen) VariantPlaylist(mediaID string, rendition string, streamType domain.StreamType) string {
	return "/" + mediaID + "/" + string(streamType) + "/" + rendition + "/playlist.m3u8"
}

func (staticPathGen) Segment(mediaID string, rendition string, streamType domain.StreamType, index int) string {
	return "/" + mediaID + "/" + string(streamType) + "/" + rendition + "/segment-" + strconv.Itoa(index) + ".ts"
}

func (staticPathGen) SpriteVTT(mediaID string) string { return "/" + mediaID + "/sprites.vtt" }
func (staticPathGen) Sprite(mediaID string, index int) string {
	return "/" + mediaID + "/sprites/" + strconv.Itoa(index)
}
func (staticPathGen) SubtitleVTT(mediaID string, lang string) string {
	return "/" + mediaID + "/subtitles/" + lang + ".vtt"
}

func TestGenerator_MasterIncludesAudioGroupsAndVariants(t *testing.T) {
	gen := NewGenerator(staticPathGen{})

	videos := []domain.VideoRendition{
		{Name: "1080p", Width: 1920, Height: 1080, Bitrate: 5_000_000},
		{Name: "480p", Width: 854, Height: 480, Bitrate: 900_000},
	}

	audios := []domain.AudioRendition{
		{Name: "aac_stereo", Codec: "aac"},
		{Name: "ac3_passthrough", Codec: "ac3"},
	}

	out := gen.Master("media", videos, audios)

	if !strings.Contains(out, "#EXTM3U") || !strings.Contains(out, "#EXT-X-VERSION:4") {
		t.Fatalf("missing mandatory headers: %s", out)
	}

	if !strings.Contains(out, "DEFAULT=YES") || !strings.Contains(out, "NAME=\"aac_stereo\"") {
		t.Fatalf("expected aac_stereo to be default audio: %s", out)
	}

	if !strings.Contains(out, "DEFAULT=NO") || !strings.Contains(out, "NAME=\"ac3_passthrough\"") {
		t.Fatalf("expected additional audio track to be non-default: %s", out)
	}

	if !strings.Contains(out, "CODECS=\"avc1.640028,mp4a.40.2\"") {
		t.Fatalf("expected 1080p codec string: %s", out)
	}

	if !strings.Contains(out, "/media/video/1080p/playlist.m3u8") || !strings.Contains(out, "/media/video/480p/playlist.m3u8") {
		t.Fatalf("video variant URIs missing: %s", out)
	}
}

func TestGenerator_VariantUsesCeilTargetDurationAndAppendsEndlist(t *testing.T) {
	gen := NewGenerator(staticPathGen{})
	mediaID := "media"
	rendition := "720p"

	segments := []domain.Segment{
		{Index: 0, Duration: 5.5},
		{Index: 1, Duration: 6.2},
	}

	out := gen.Variant(mediaID, rendition, domain.StreamVideo, segments)

	if !strings.Contains(out, "#EXT-X-TARGETDURATION:7") {
		t.Fatalf("target duration should ceil max segment: %s", out)
	}
	if strings.Count(out, "#EXTINF") != len(segments) {
		t.Fatalf("expected %d EXTINF lines, got %d", len(segments), strings.Count(out, "#EXTINF"))
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "#EXT-X-ENDLIST") {
		t.Fatalf("playlist should terminate with ENDLIST: %s", out)
	}

	for _, seg := range segments {
		want := "/media/video/" + rendition + "/segment-" + strconv.Itoa(seg.Index) + ".ts"
		if !strings.Contains(out, want) {
			t.Fatalf("missing segment URI %s in output: %s", want, out)
		}
	}
}
