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
func (staticPathGen) SubtitlePlaylist(mediaID string, trackID string) string {
	return "/" + mediaID + "/subtitles/" + trackID + "/playlist.m3u8"
}
func (staticPathGen) SubtitleVTT(mediaID string, lang string) string {
	return "/" + mediaID + "/subtitles/" + lang + ".vtt"
}
func (staticPathGen) Preview(mediaID string) string {
	return "/" + mediaID + "/preview.gif"
}
func (staticPathGen) Thumbnail(mediaID string) string {
	return "/" + mediaID + "/thumbnail.jpg"
}

func TestGenerator_MasterIncludesAudioGroupsAndVariants(t *testing.T) {
	gen := NewGenerator(staticPathGen{})

	videos := []domain.VideoRendition{
		{Name: "1080p", Width: 1920, Height: 1080, Bitrate: 5_000_000},
		{Name: "480p", Width: 854, Height: 480, Bitrate: 900_000},
	}

	audios := []domain.AudioRendition{
		{Name: "aac_stereo", Codec: "aac", Default: true},
		{Name: "ac3_passthrough", Codec: "ac3"},
	}

	out := gen.Master("media", videos, audios, nil)

	if !strings.Contains(out, "#EXTM3U") || !strings.Contains(out, "#EXT-X-VERSION:4") {
		t.Fatalf("missing mandatory headers: %s", out)
	}

	if !strings.Contains(out, "DEFAULT=YES") || !strings.Contains(out, "NAME=\"aac_stereo\"") {
		t.Fatalf("expected aac_stereo to be default audio: %s", out)
	}

	if strings.Count(out, "DEFAULT=YES") != 2 || !strings.Contains(out, "NAME=\"ac3_passthrough\"") {
		t.Fatalf("expected a default rendition in each codec-compatible group: %s", out)
	}

	if !strings.Contains(out, "CODECS=\"avc1.64002a,mp4a.40.2\"") {
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

func TestGenerator_MasterOmitsAudioForSilentVideo(t *testing.T) {
	gen := NewGenerator(staticPathGen{})
	videos := []domain.VideoRendition{{Name: "720p", Width: 1280, Height: 720, Bitrate: 2_000_000, FrameRate: 24, Codec: "avc1.64001f"}}

	out := gen.Master("media", videos, nil, nil)

	if strings.Contains(out, "AUDIO=") || strings.Contains(out, "mp4a.40.2") || strings.Contains(out, "TYPE=AUDIO") {
		t.Fatalf("silent video must not advertise audio: %s", out)
	}
	if !strings.Contains(out, "AVERAGE-BANDWIDTH=2000000") || !strings.Contains(out, "FRAME-RATE=24.000") {
		t.Fatalf("missing video attributes: %s", out)
	}
}

func TestGenerator_MasterDeclaresAudioCompatibility(t *testing.T) {
	gen := NewGenerator(staticPathGen{})
	videos := []domain.VideoRendition{{Name: "1080p", Width: 1920, Height: 1080, Bitrate: 5_000_000, PeakBitrate: 7_500_000, FrameRate: 23.976, Codec: "avc1.640028"}}
	audios := []domain.AudioRendition{
		{Name: "english", Codec: "aac", CodecString: "mp4a.40.2", Bitrate: 128_000, Channels: 2, Language: "eng", Default: true},
		{Name: "spanish", Codec: "ac3", CodecString: "ac-3", Bitrate: 384_000, Channels: 6, Language: "spa"},
	}

	out := gen.Master("media", videos, audios, nil)

	for _, value := range []string{"LANGUAGE=\"eng\"", "CHANNELS=\"2\"", "LANGUAGE=\"spa\"", "CHANNELS=\"6\"", "CODECS=\"avc1.640028,ac-3\"", "BANDWIDTH=7884000"} {
		if !strings.Contains(out, value) {
			t.Fatalf("expected %q in master playlist: %s", value, out)
		}
	}
}

func TestGenerator_MasterDeclaresSubtitleTracks(t *testing.T) {
	gen := NewGenerator(staticPathGen{})
	videos := []domain.VideoRendition{{Name: "720p", Width: 1280, Height: 720, Bitrate: 2_000_000, Codec: "avc1.64001f"}}
	subtitles := []domain.SubtitleStream{{ID: "subtitle-4", Language: "eng", Title: "English", Default: true}}

	out := gen.Master("media", videos, nil, subtitles)

	for _, want := range []string{"TYPE=SUBTITLES", "GROUP-ID=\"subtitles\"", "NAME=\"English\"", "LANGUAGE=\"eng\"", "SUBTITLES=\"subtitles\"", "/media/subtitles/subtitle-4/playlist.m3u8"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in master playlist: %s", want, out)
		}
	}
}
