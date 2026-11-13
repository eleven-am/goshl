package playlist

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/Eyevinn/hls-m3u8/m3u8"

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

func buildMaster(t *testing.T, videos []domain.VideoRendition, audios []domain.AudioRendition, subtitles []domain.SubtitleStream) string {
	t.Helper()
	out, err := NewGenerator(staticPathGen{}).Master("media", videos, audios, subtitles)
	if err != nil {
		t.Fatalf("master: %v", err)
	}
	return out
}

func buildVariant(t *testing.T, rendition string, streamType domain.StreamType, segments []domain.Segment) (string, error) {
	t.Helper()
	return NewGenerator(staticPathGen{}).Variant("media", rendition, streamType, segments)
}

func buildSubtitle(t *testing.T, trackID string, duration float64) (string, error) {
	t.Helper()
	return NewGenerator(staticPathGen{}).Subtitle("media", trackID, duration)
}

type parsedMaster struct {
	raw       string
	playlist  *m3u8.MasterPlaylist
	audio     map[string][]*m3u8.Alternative
	subtitles map[string][]*m3u8.Alternative
}

func parseMaster(t *testing.T, out string) parsedMaster {
	t.Helper()
	decoded, listType, err := m3u8.DecodeFrom(strings.NewReader(out), true)
	if err != nil {
		t.Fatalf("decode master: %v\n%s", err, out)
	}
	if listType != m3u8.MASTER {
		t.Fatalf("expected a master playlist, got list type %v\n%s", listType, out)
	}
	parsed := parsedMaster{
		raw:       out,
		playlist:  decoded.(*m3u8.MasterPlaylist),
		audio:     map[string][]*m3u8.Alternative{},
		subtitles: map[string][]*m3u8.Alternative{},
	}
	seen := map[*m3u8.Alternative]bool{}
	for _, variant := range parsed.playlist.Variants {
		for _, alternative := range variant.Alternatives {
			if seen[alternative] {
				continue
			}
			seen[alternative] = true
			switch alternative.Type {
			case "AUDIO":
				parsed.audio[alternative.GroupId] = append(parsed.audio[alternative.GroupId], alternative)
			case "SUBTITLES":
				parsed.subtitles[alternative.GroupId] = append(parsed.subtitles[alternative.GroupId], alternative)
			default:
				t.Fatalf("unexpected rendition type %q\n%s", alternative.Type, out)
			}
		}
	}
	if declared := strings.Count(out, "#EXT-X-MEDIA:"); declared != len(seen) {
		t.Fatalf("%d EXT-X-MEDIA tags declared but %d are referenced by a variant\n%s", declared, len(seen), out)
	}
	return parsed
}

func memberSignature(alternative *m3u8.Alternative) string {
	return strings.Join([]string{
		alternative.Name,
		alternative.Language,
		strconv.FormatBool(alternative.Default),
		strconv.FormatBool(alternative.Autoselect),
		strconv.FormatBool(alternative.Forced),
	}, "|")
}

func assertGroupsConform(t *testing.T, raw string, kind string, groups map[string][]*m3u8.Alternative) {
	t.Helper()
	var reference []string
	var referenceID string
	for id, members := range groups {
		names := map[string]bool{}
		autoselected := map[string]bool{}
		defaults := 0
		signatures := make([]string, 0, len(members))
		for _, member := range members {
			if member.Name == "" {
				t.Fatalf("%s group %q has a member without NAME\n%s", kind, id, raw)
			}
			if names[member.Name] {
				t.Fatalf("%s group %q repeats NAME %q\n%s", kind, id, member.Name, raw)
			}
			names[member.Name] = true
			if member.Default {
				defaults++
				if !member.Autoselect {
					t.Fatalf("%s group %q member %q is DEFAULT without AUTOSELECT\n%s", kind, id, member.Name, raw)
				}
			}
			if member.Forced && !member.Autoselect {
				t.Fatalf("%s group %q member %q is FORCED without AUTOSELECT\n%s", kind, id, member.Name, raw)
			}
			if member.Autoselect && !member.Forced {
				if autoselected[member.Language] {
					t.Fatalf("%s group %q has two AUTOSELECT members for language %q\n%s", kind, id, member.Language, raw)
				}
				autoselected[member.Language] = true
			}
			if member.URI == "" {
				t.Fatalf("%s group %q member %q has no URI\n%s", kind, id, member.Name, raw)
			}
			signatures = append(signatures, memberSignature(member))
		}
		if defaults > 1 {
			t.Fatalf("%s group %q has %d DEFAULT=YES members\n%s", kind, id, defaults, raw)
		}
		if reference == nil {
			reference, referenceID = signatures, id
			continue
		}
		if strings.Join(reference, ";") != strings.Join(signatures, ";") {
			t.Fatalf("%s groups %q and %q differ in members:\n%v\n%v\n%s", kind, referenceID, id, reference, signatures, raw)
		}
	}
}

func assertMasterConforms(t *testing.T, out string) parsedMaster {
	t.Helper()
	parsed := parseMaster(t, out)
	if len(parsed.playlist.Variants) == 0 {
		t.Fatalf("master has no variants\n%s", out)
	}
	if required, reason := parsed.playlist.CalcMinVersion(); parsed.playlist.Version() < required {
		t.Fatalf("EXT-X-VERSION %d is below the required %d (%s)\n%s", parsed.playlist.Version(), required, reason, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "#EXT-X-MEDIA:TYPE=AUDIO") && strings.Contains(line, "FORCED=") {
			t.Fatalf("FORCED present on an audio rendition: %s", line)
		}
	}
	assertGroupsConform(t, out, "audio", parsed.audio)
	assertGroupsConform(t, out, "subtitle", parsed.subtitles)
	for _, variant := range parsed.playlist.Variants {
		if variant.Bandwidth == 0 {
			t.Fatalf("variant %q has no BANDWIDTH\n%s", variant.URI, out)
		}
		if variant.AverageBandwidth > variant.Bandwidth {
			t.Fatalf("variant %q AVERAGE-BANDWIDTH exceeds BANDWIDTH\n%s", variant.URI, out)
		}
		if variant.Codecs == "" {
			t.Fatalf("variant %q has no CODECS\n%s", variant.URI, out)
		}
		if variant.URI == "" {
			t.Fatalf("variant without URI\n%s", out)
		}
		if variant.Audio != "" && len(parsed.audio[variant.Audio]) == 0 {
			t.Fatalf("variant %q references missing audio group %q\n%s", variant.URI, variant.Audio, out)
		}
		if variant.Subtitles != "" && len(parsed.subtitles[variant.Subtitles]) == 0 {
			t.Fatalf("variant %q references missing subtitle group %q\n%s", variant.URI, variant.Subtitles, out)
		}
	}
	return parsed
}

func memberByName(t *testing.T, members []*m3u8.Alternative, name string) *m3u8.Alternative {
	t.Helper()
	for _, member := range members {
		if member.Name == name {
			return member
		}
	}
	t.Fatalf("no member named %q in %d members", name, len(members))
	return nil
}

func variantByURI(t *testing.T, parsed parsedMaster, uri string, audioGroup string) *m3u8.Variant {
	t.Helper()
	for _, variant := range parsed.playlist.Variants {
		if variant.URI == uri && variant.Audio == audioGroup {
			return variant
		}
	}
	t.Fatalf("no variant %q with audio group %q\n%s", uri, audioGroup, parsed.raw)
	return nil
}

func video1080() domain.VideoRendition {
	return domain.VideoRendition{Name: "1080p", Width: 1920, Height: 1080, Bitrate: 5_000_000, PeakBitrate: 7_500_000, FrameRate: 23.976, Codec: "avc1.640028", Level: 40, Method: domain.Transcode}
}

func video480() domain.VideoRendition {
	return domain.VideoRendition{Name: "480p", Width: 854, Height: 480, Bitrate: 900_000, PeakBitrate: 1_350_000, FrameRate: 23.976, Codec: "avc1.64001f", Level: 31, Method: domain.Transcode}
}

func twoTrackAudio() []domain.AudioRendition {
	return []domain.AudioRendition{
		{Name: "audio0_aac_stereo", Codec: "aac", CodecString: "mp4a.40.2", Bitrate: 128_000, Channels: 2, StreamIndex: 0, Language: "eng", Default: true, Method: domain.Transcode},
		{Name: "audio1_aac_stereo", Codec: "aac", CodecString: "mp4a.40.2", Bitrate: 128_000, Channels: 2, StreamIndex: 1, Language: "spa", Default: true, Method: domain.Transcode},
		{Name: "audio1_aac_surround", Codec: "aac", CodecString: "mp4a.40.2", Bitrate: 384_000, Channels: 6, StreamIndex: 1, Language: "spa", Method: domain.Transcode},
		{Name: "audio1_ac3_passthrough", Codec: "ac3", CodecString: "ac-3", Bitrate: 640_000, Channels: 6, StreamIndex: 1, Language: "spa", Method: domain.DirectStream},
	}
}

func TestMasterHasExactlyOneDefaultPerAudioGroup(t *testing.T) {
	out := buildMaster(t, []domain.VideoRendition{video1080(), video480()}, twoTrackAudio(), nil)
	parsed := assertMasterConforms(t, out)

	if len(parsed.audio) != 3 {
		t.Fatalf("expected 3 audio groups, got %d\n%s", len(parsed.audio), out)
	}
	for id, members := range parsed.audio {
		var defaults []string
		for _, member := range members {
			if member.Default {
				defaults = append(defaults, member.Language)
			}
		}
		if len(defaults) != 1 || defaults[0] != "en" {
			t.Fatalf("group %q: expected exactly the eng track as DEFAULT, got %v\n%s", id, defaults, out)
		}
	}
}

func TestMasterAudioDefaultFollowsRenditionDefault(t *testing.T) {
	audios := twoTrackAudio()
	audios[0].Default = false
	out := buildMaster(t, []domain.VideoRendition{video1080()}, audios, nil)
	parsed := assertMasterConforms(t, out)

	for id, members := range parsed.audio {
		for _, member := range members {
			if member.Default != (member.Language == "es") {
				t.Fatalf("group %q member %q DEFAULT=%v, want the spa track only\n%s", id, member.Name, member.Default, out)
			}
		}
	}
}

func TestMasterAudioDefaultsToFirstTrack(t *testing.T) {
	audios := twoTrackAudio()
	audios[0].Default = false
	audios[1].Default = false
	out := buildMaster(t, []domain.VideoRendition{video1080()}, audios, nil)
	parsed := assertMasterConforms(t, out)

	for id, members := range parsed.audio {
		for _, member := range members {
			if member.Default != (member.Language == "en") {
				t.Fatalf("group %q member %q DEFAULT=%v, want the first track only\n%s", id, member.Name, member.Default, out)
			}
		}
	}
}

func surroundTrack(streamIndex int, language string, passthrough string, codecString string, channels int) []domain.AudioRendition {
	prefix := "audio" + strconv.Itoa(streamIndex) + "_"
	return []domain.AudioRendition{
		{Name: prefix + "aac_stereo", Codec: "aac", CodecString: "mp4a.40.2", Bitrate: 128_000, Channels: 2, StreamIndex: streamIndex, Language: language, Default: streamIndex == 0},
		{Name: prefix + "aac_surround", Codec: "aac", CodecString: "mp4a.40.2", Bitrate: 384_000, Channels: 6, StreamIndex: streamIndex, Language: language},
		{Name: prefix + passthrough + "_passthrough", Codec: passthrough, CodecString: codecString, Bitrate: 640_000, Channels: channels, StreamIndex: streamIndex, Language: language},
	}
}

func TestMasterAudioFallbackMembers(t *testing.T) {
	var audios []domain.AudioRendition
	audios = append(audios, surroundTrack(0, "eng", "eac3", "ec-3", 6)...)
	audios = append(audios, surroundTrack(1, "spa", "ac3", "ac-3", 6)...)
	audios = append(audios, surroundTrack(2, "fra", "eac3", "ec-3", 8)...)
	audios = append(audios,
		domain.AudioRendition{Name: "audio3_aac_stereo", Codec: "aac", CodecString: "mp4a.40.2", Bitrate: 128_000, Channels: 2, StreamIndex: 3, Language: "deu"},
		domain.AudioRendition{Name: "audio3_ac3_passthrough", Codec: "ac3", CodecString: "ac-3", Bitrate: 192_000, Channels: 1, StreamIndex: 3, Language: "deu"},
	)
	out := buildMaster(t, []domain.VideoRendition{video1080()}, audios, nil)
	parsed := assertMasterConforms(t, out)

	want := map[string]map[string]string{
		"audio-mp4a-40-2-2": {"eng": "audio0_aac_stereo", "spa": "audio1_aac_stereo", "fra": "audio2_aac_stereo", "deu": "audio3_aac_stereo"},
		"audio-mp4a-40-2-6": {"eng": "audio0_aac_surround", "spa": "audio1_aac_surround", "fra": "audio2_aac_surround", "deu": "audio3_aac_stereo"},
		"audio-ec-3-6":      {"eng": "audio0_eac3_passthrough", "spa": "audio1_aac_surround", "fra": "audio2_aac_surround", "deu": "audio3_aac_stereo"},
		"audio-ec-3-8":      {"eng": "audio0_eac3_passthrough", "spa": "audio1_aac_surround", "fra": "audio2_eac3_passthrough", "deu": "audio3_aac_stereo"},
		"audio-ac-3-6":      {"eng": "audio0_aac_surround", "spa": "audio1_ac3_passthrough", "fra": "audio2_aac_surround", "deu": "audio3_ac3_passthrough"},
		"audio-ac-3-1":      {"eng": "audio0_aac_stereo", "spa": "audio1_aac_stereo", "fra": "audio2_aac_stereo", "deu": "audio3_ac3_passthrough"},
	}
	if len(parsed.audio) != len(want) {
		t.Fatalf("expected %d audio groups, got %d\n%s", len(want), len(parsed.audio), out)
	}
	for id, renditions := range want {
		for language, rendition := range renditions {
			wantURI := "/media/audio/" + rendition + "/playlist.m3u8"
			if got := memberByName(t, parsed.audio[id], language).URI; got != wantURI {
				t.Fatalf("group %q member %q URI = %q, want %q", id, language, got, wantURI)
			}
		}
	}
}

func TestMasterAudioGroupsShareMembers(t *testing.T) {
	out := buildMaster(t, []domain.VideoRendition{video1080()}, twoTrackAudio(), nil)
	parsed := assertMasterConforms(t, out)

	want := map[string]map[string]string{
		"audio-mp4a-40-2-2": {"eng": "/media/audio/audio0_aac_stereo/playlist.m3u8", "spa": "/media/audio/audio1_aac_stereo/playlist.m3u8"},
		"audio-mp4a-40-2-6": {"eng": "/media/audio/audio0_aac_stereo/playlist.m3u8", "spa": "/media/audio/audio1_aac_surround/playlist.m3u8"},
		"audio-ac-3-6":      {"eng": "/media/audio/audio0_aac_stereo/playlist.m3u8", "spa": "/media/audio/audio1_ac3_passthrough/playlist.m3u8"},
	}
	for id, uris := range want {
		members := parsed.audio[id]
		if len(members) != len(uris) {
			t.Fatalf("group %q has %d members, want %d\n%s", id, len(members), len(uris), out)
		}
		for name, uri := range uris {
			if got := memberByName(t, members, name).URI; got != uri {
				t.Fatalf("group %q member %q URI = %q, want %q", id, name, got, uri)
			}
		}
	}

	if got := memberByName(t, parsed.audio["audio-ac-3-6"], "eng").Channels.Amount; got != 2 {
		t.Fatalf("fallback member must keep its own channel count, got %d", got)
	}
	mixed := variantByURI(t, parsed, "/media/video/1080p/playlist.m3u8", "audio-ac-3-6")
	if mixed.Codecs != "avc1.640028,ac-3,mp4a.40.2" {
		t.Fatalf("mixed group CODECS = %q\n%s", mixed.Codecs, out)
	}
	if mixed.Bandwidth != 7_500_000+640_000 || mixed.AverageBandwidth != 5_000_000+640_000 {
		t.Fatalf("mixed group bandwidth = %d/%d\n%s", mixed.AverageBandwidth, mixed.Bandwidth, out)
	}
	stereo := variantByURI(t, parsed, "/media/video/1080p/playlist.m3u8", "audio-mp4a-40-2-2")
	if stereo.Codecs != "avc1.640028,mp4a.40.2" || stereo.Bandwidth != 7_500_000+128_000 {
		t.Fatalf("stereo variant = %q %d\n%s", stereo.Codecs, stereo.Bandwidth, out)
	}
	if stereo.Resolution != "1920x1080" || math.Abs(stereo.FrameRate-23.976) > 0.0005 {
		t.Fatalf("stereo variant resolution/frame rate = %q %v", stereo.Resolution, stereo.FrameRate)
	}
}

func TestMasterAudioNamesUniqueAndAutoselectPerLanguage(t *testing.T) {
	audios := []domain.AudioRendition{
		{Name: "audio0_aac_stereo", Codec: "aac", CodecString: "mp4a.40.2", Bitrate: 128_000, Channels: 2, StreamIndex: 0, Language: "eng", Default: true},
		{Name: "audio1_aac_stereo", Codec: "aac", CodecString: "mp4a.40.2", Bitrate: 128_000, Channels: 2, StreamIndex: 1, Language: "eng"},
		{Name: "audio2_aac_stereo", Codec: "aac", CodecString: "mp4a.40.2", Bitrate: 128_000, Channels: 2, StreamIndex: 2},
		{Name: "audio3_aac_stereo", Codec: "aac", CodecString: "mp4a.40.2", Bitrate: 128_000, Channels: 2, StreamIndex: 3},
	}
	out := buildMaster(t, []domain.VideoRendition{video1080()}, audios, nil)
	parsed := assertMasterConforms(t, out)

	members := parsed.audio["audio-mp4a-40-2-2"]
	if len(members) != 4 {
		t.Fatalf("expected 4 members, got %d\n%s", len(members), out)
	}
	want := []struct {
		name, language      string
		isDefault, autoPick bool
	}{
		{"eng", "en", true, true},
		{"eng 2", "en", false, false},
		{"Audio 3", "", false, true},
		{"Audio 4", "", false, false},
	}
	for index, expected := range want {
		got := members[index]
		if got.Name != expected.name || got.Language != expected.language || got.Default != expected.isDefault || got.Autoselect != expected.autoPick {
			t.Fatalf("member %d = %+v, want %+v\n%s", index, *got, expected, out)
		}
	}
}

func TestMasterSubtitleNamesUnique(t *testing.T) {
	subtitles := []domain.SubtitleStream{
		{ID: "subtitle-2", Language: "eng"},
		{ID: "subtitle-3", Language: "eng"},
		{ID: "subtitle-4", Language: "eng", Title: "eng 2"},
		{ID: "subtitle-5", Title: "Director's \"cut\""},
		{ID: "subtitle-6", Title: "Director's 'cut'"},
		{ID: "subtitle-7"},
	}
	out := buildMaster(t, []domain.VideoRendition{video1080()}, nil, subtitles)
	parsed := assertMasterConforms(t, out)

	members := parsed.subtitles["subtitles"]
	if len(members) != len(subtitles) {
		t.Fatalf("expected %d subtitle renditions, got %d\n%s", len(subtitles), len(members), out)
	}
	want := []string{"eng", "eng 2", "eng 2 2", "Director's 'cut'", "Director's 'cut' 2", "subtitle-7"}
	for index, name := range want {
		if members[index].Name != name {
			t.Fatalf("subtitle %d NAME = %q, want %q\n%s", index, members[index].Name, name, out)
		}
		if wantURI := "/media/subtitles/" + subtitles[index].ID + "/playlist.m3u8"; members[index].URI != wantURI {
			t.Fatalf("subtitle %d URI = %q, want %q", index, members[index].URI, wantURI)
		}
	}
}

func TestMasterSubtitleDefaultFollowsDisposition(t *testing.T) {
	cases := []struct {
		name      string
		subtitles []domain.SubtitleStream
		want      []bool
	}{
		{"none flagged", []domain.SubtitleStream{{ID: "a", Language: "eng"}, {ID: "b", Language: "fra"}}, []bool{false, false}},
		{"second flagged", []domain.SubtitleStream{{ID: "a", Language: "eng"}, {ID: "b", Language: "fra", Default: true}}, []bool{false, true}},
		{"two flagged", []domain.SubtitleStream{{ID: "a", Language: "eng", Default: true}, {ID: "b", Language: "fra", Default: true}}, []bool{true, false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := buildMaster(t, []domain.VideoRendition{video1080()}, nil, tc.subtitles)
			parsed := assertMasterConforms(t, out)
			members := parsed.subtitles["subtitles"]
			if len(members) != len(tc.want) {
				t.Fatalf("expected %d subtitle renditions, got %d\n%s", len(tc.want), len(members), out)
			}
			for index, want := range tc.want {
				if members[index].Default != want {
					t.Fatalf("subtitle %d DEFAULT = %v, want %v\n%s", index, members[index].Default, want, out)
				}
			}
		})
	}
}

func TestMasterSubtitleForcedAndAutoselect(t *testing.T) {
	subtitles := []domain.SubtitleStream{
		{ID: "full", Language: "eng", Title: "English"},
		{ID: "forced", Language: "eng", Title: "English forced", Forced: true},
		{ID: "sdh", Language: "eng", Title: "English SDH", Default: true},
		{ID: "signs", Language: "eng", Title: "English signs", Forced: true},
		{ID: "french", Language: "fra", Title: "Français"},
	}
	out := buildMaster(t, []domain.VideoRendition{video1080()}, twoTrackAudio(), subtitles)
	parsed := assertMasterConforms(t, out)

	members := parsed.subtitles["subtitles"]
	want := map[string][3]bool{
		"English":        {false, false, false},
		"English forced": {false, true, true},
		"English SDH":    {true, true, false},
		"English signs":  {false, true, true},
		"Français":       {false, true, false},
	}
	for name, flags := range want {
		got := memberByName(t, members, name)
		if got.Default != flags[0] || got.Autoselect != flags[1] || got.Forced != flags[2] {
			t.Fatalf("%q DEFAULT/AUTOSELECT/FORCED = %v/%v/%v, want %v\n%s", name, got.Default, got.Autoselect, got.Forced, flags, out)
		}
	}
	for _, variant := range parsed.playlist.Variants {
		if variant.Subtitles != "subtitles" {
			t.Fatalf("variant %q does not reference the subtitle group\n%s", variant.URI, out)
		}
	}
}

func TestAudioOnlyMasterAdvertisesBandwidthAndSubtitles(t *testing.T) {
	audios := twoTrackAudio()
	audios[0].Default = false
	subtitles := []domain.SubtitleStream{{ID: "subtitle-2", Language: "eng", Title: "English"}}
	out := buildMaster(t, nil, audios, subtitles)
	parsed := assertMasterConforms(t, out)

	want := map[string]struct {
		uri       string
		bandwidth uint32
		codecs    string
	}{
		"audio-mp4a-40-2-2": {"/media/audio/audio1_aac_stereo/playlist.m3u8", 128_000, "mp4a.40.2"},
		"audio-mp4a-40-2-6": {"/media/audio/audio1_aac_surround/playlist.m3u8", 384_000, "mp4a.40.2"},
		"audio-ac-3-6":      {"/media/audio/audio1_ac3_passthrough/playlist.m3u8", 640_000, "ac-3,mp4a.40.2"},
	}
	if len(parsed.playlist.Variants) != len(want) {
		t.Fatalf("expected one variant per audio group, got %d\n%s", len(parsed.playlist.Variants), out)
	}
	for _, variant := range parsed.playlist.Variants {
		expected, ok := want[variant.Audio]
		if !ok {
			t.Fatalf("variant %q references unexpected audio group %q\n%s", variant.URI, variant.Audio, out)
		}
		if variant.URI != expected.uri || variant.Bandwidth != expected.bandwidth || variant.AverageBandwidth != expected.bandwidth || variant.Codecs != expected.codecs {
			t.Fatalf("group %q variant = %q %d %q, want %+v\n%s", variant.Audio, variant.URI, variant.Bandwidth, variant.Codecs, expected, out)
		}
		if variant.Subtitles != "subtitles" {
			t.Fatalf("audio-only variant %q does not reference the subtitle group\n%s", variant.URI, out)
		}
		if variant.Resolution != "" {
			t.Fatalf("audio-only variant %q carries RESOLUTION %q", variant.URI, variant.Resolution)
		}
	}
	if len(parsed.subtitles["subtitles"]) != 1 {
		t.Fatalf("expected the subtitle rendition to be reachable\n%s", out)
	}
}

func TestMasterOmitsAudioForSilentVideo(t *testing.T) {
	out := buildMaster(t, []domain.VideoRendition{video1080(), video480()}, nil, nil)
	parsed := assertMasterConforms(t, out)

	if strings.Contains(out, "#EXT-X-MEDIA") {
		t.Fatalf("silent video must not declare renditions\n%s", out)
	}
	if len(parsed.playlist.Variants) != 2 {
		t.Fatalf("expected 2 variants, got %d\n%s", len(parsed.playlist.Variants), out)
	}
	top := variantByURI(t, parsed, "/media/video/1080p/playlist.m3u8", "")
	if top.Codecs != "avc1.640028" || top.Bandwidth != 7_500_000 || top.AverageBandwidth != 5_000_000 || top.Subtitles != "" {
		t.Fatalf("unexpected silent variant %+v\n%s", top.VariantParams, out)
	}
	low := variantByURI(t, parsed, "/media/video/480p/playlist.m3u8", "")
	if low.Codecs != "avc1.64001f" || low.Resolution != "854x480" {
		t.Fatalf("unexpected silent variant %+v\n%s", low.VariantParams, out)
	}
}

func TestMasterVideoCodecsComeFromRendition(t *testing.T) {
	transcoded := video1080()
	transcoded.Codec = "avc1.640033"
	direct := video480()
	direct.Codec = "avc1.42001e"
	direct.Method = domain.DirectStream
	out := buildMaster(t, []domain.VideoRendition{transcoded, direct}, nil, nil)
	parsed := assertMasterConforms(t, out)

	if got := variantByURI(t, parsed, "/media/video/1080p/playlist.m3u8", "").Codecs; got != "avc1.640033" {
		t.Fatalf("transcode CODECS = %q, want the rendition's avc1.640033", got)
	}
	if got := variantByURI(t, parsed, "/media/video/480p/playlist.m3u8", "").Codecs; got != "avc1.42001e" {
		t.Fatalf("direct stream CODECS = %q, want the rendition's avc1.42001e", got)
	}
}

func TestMasterRejectsInvalidRenditions(t *testing.T) {
	gen := NewGenerator(staticPathGen{})

	if _, err := gen.Master("media", nil, nil, []domain.SubtitleStream{{ID: "s"}}); !errors.Is(err, ErrNoRenditions) {
		t.Fatalf("no renditions: err = %v, want ErrNoRenditions", err)
	}

	noVideoCodec := video1080()
	noVideoCodec.Codec = ""
	noVideoBitrate := video1080()
	noVideoBitrate.Bitrate = 0
	noAudioCodec := twoTrackAudio()
	noAudioCodec[3].CodecString = ""
	noAudioBitrate := twoTrackAudio()
	noAudioBitrate[3].Bitrate = 0
	sameGroup := twoTrackAudio()
	sameGroup[2].Channels = 2

	cases := []struct {
		name   string
		videos []domain.VideoRendition
		audios []domain.AudioRendition
	}{
		{"video codec", []domain.VideoRendition{noVideoCodec}, nil},
		{"video bitrate", []domain.VideoRendition{noVideoBitrate}, nil},
		{"audio codec", []domain.VideoRendition{video1080()}, noAudioCodec},
		{"audio bitrate", nil, noAudioBitrate},
		{"two renditions of one stream in one group", []domain.VideoRendition{video1080()}, sameGroup},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := gen.Master("media", tc.videos, tc.audios, nil)
			if !errors.Is(err, ErrInvalidRendition) {
				t.Fatalf("err = %v, want ErrInvalidRendition; output:\n%s", err, out)
			}
			if out != "" {
				t.Fatalf("a rejected master must not return a playlist:\n%s", out)
			}
		})
	}
}

func parseMedia(t *testing.T, out string) *m3u8.MediaPlaylist {
	t.Helper()
	decoded, listType, err := m3u8.DecodeFrom(strings.NewReader(out), true)
	if err != nil {
		t.Fatalf("decode media playlist: %v\n%s", err, out)
	}
	if listType != m3u8.MEDIA {
		t.Fatalf("expected a media playlist, got list type %v\n%s", listType, out)
	}
	media := decoded.(*m3u8.MediaPlaylist)
	if !media.Closed || media.MediaType != m3u8.VOD || media.SeqNo != 0 {
		t.Fatalf("expected a closed VOD playlist starting at 0: closed=%v type=%v seq=%d\n%s", media.Closed, media.MediaType, media.SeqNo, out)
	}
	target := declaredTargetDuration(t, out)
	if target <= 0 {
		t.Fatalf("EXT-X-TARGETDURATION is %d\n%s", target, out)
	}
	for _, segment := range media.GetAllSegments() {
		if rounded := math.Round(segment.Duration); rounded > float64(target) {
			t.Fatalf("segment %q EXTINF %v rounds above TARGETDURATION %d\n%s", segment.URI, segment.Duration, target, out)
		}
	}
	return media
}

func declaredTargetDuration(t *testing.T, out string) int {
	t.Helper()
	const tag = "#EXT-X-TARGETDURATION:"
	var values []int
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, tag) {
			continue
		}
		value, err := strconv.Atoi(strings.TrimPrefix(line, tag))
		if err != nil {
			t.Fatalf("EXT-X-TARGETDURATION is not a decimal-integer: %q", line)
		}
		values = append(values, value)
	}
	if len(values) != 1 {
		t.Fatalf("expected exactly one EXT-X-TARGETDURATION tag, got %d\n%s", len(values), out)
	}
	return values[0]
}

func TestVariantDeclaresEverySegment(t *testing.T) {
	segments := []domain.Segment{
		{Index: 0, Duration: 5.5},
		{Index: 1, Duration: 6.2},
		{Index: 2, Duration: 6.4996},
		{Index: 3, Duration: 0.0424},
	}
	out, err := buildVariant(t, "720p", domain.StreamVideo, segments)
	if err != nil {
		t.Fatalf("variant: %v", err)
	}
	media := parseMedia(t, out)

	if target := declaredTargetDuration(t, out); target != 7 {
		t.Fatalf("TARGETDURATION = %d, want ceil(6.4996) = 7\n%s", target, out)
	}
	decoded := media.GetAllSegments()
	if len(decoded) != len(segments) {
		t.Fatalf("decoded %d segments, want %d\n%s", len(decoded), len(segments), out)
	}
	for index, segment := range segments {
		wantURI := "/media/video/720p/segment-" + strconv.Itoa(segment.Index) + ".ts"
		if decoded[index].URI != wantURI {
			t.Fatalf("segment %d URI = %q, want %q", index, decoded[index].URI, wantURI)
		}
		if math.Abs(decoded[index].Duration-segment.Duration) > 0.0005 {
			t.Fatalf("segment %d EXTINF = %v, want %v within 0.5 ms", index, decoded[index].Duration, segment.Duration)
		}
	}
}

func TestVariantRejectsEmptyPlan(t *testing.T) {
	for _, segments := range [][]domain.Segment{nil, {}} {
		out, err := buildVariant(t, "720p", domain.StreamVideo, segments)
		if !errors.Is(err, ErrEmptyPlan) {
			t.Fatalf("err = %v, want ErrEmptyPlan; output:\n%s", err, out)
		}
		if out != "" {
			t.Fatalf("a rejected plan must not return a playlist:\n%s", out)
		}
	}
}

func TestVariantRejectsNonPositiveDurations(t *testing.T) {
	for _, duration := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		segments := []domain.Segment{{Index: 0, Duration: 6}, {Index: 1, Duration: duration}}
		out, err := buildVariant(t, "720p", domain.StreamVideo, segments)
		if !errors.Is(err, ErrEmptyPlan) || out != "" {
			t.Fatalf("duration %v: err = %v, output %q; want ErrEmptyPlan and no playlist", duration, err, out)
		}
	}
}

func TestSubtitlePlaylistDeclaresOneSegment(t *testing.T) {
	out, err := buildSubtitle(t, "subtitle-4", 5400.25)
	if err != nil {
		t.Fatalf("subtitle playlist: %v", err)
	}
	media := parseMedia(t, out)
	decoded := media.GetAllSegments()
	if target := declaredTargetDuration(t, out); target != 5401 || len(decoded) != 1 {
		t.Fatalf("TARGETDURATION = %d with %d segments, want 5401 and 1\n%s", target, len(decoded), out)
	}
	if decoded[0].URI != "/media/subtitles/subtitle-4.vtt" || math.Abs(decoded[0].Duration-5400.25) > 0.0005 {
		t.Fatalf("unexpected subtitle segment %q %v", decoded[0].URI, decoded[0].Duration)
	}
}

func TestSubtitlePlaylistRejectsNonPositiveDuration(t *testing.T) {
	for _, duration := range []float64{0, -3, math.NaN(), math.Inf(1)} {
		out, err := buildSubtitle(t, "subtitle-4", duration)
		if !errors.Is(err, ErrEmptyPlan) || out != "" {
			t.Fatalf("duration %v: err = %v, output %q; want ErrEmptyPlan and no playlist", duration, err, out)
		}
	}
}

func TestMasterAudioNameUsesTitleAndLanguageIsRFC5646(t *testing.T) {
	audios := []domain.AudioRendition{
		{Name: "audio0_aac_stereo", Codec: "aac", CodecString: "mp4a.40.2", Bitrate: 128_000, Channels: 2, StreamIndex: 0, Language: "eng", Title: "Commentary \"cut\"", Default: true},
		{Name: "audio1_aac_stereo", Codec: "aac", CodecString: "mp4a.40.2", Bitrate: 128_000, Channels: 2, StreamIndex: 1, Language: "jpn", Title: "Commentary 'cut'"},
		{Name: "audio2_aac_stereo", Codec: "aac", CodecString: "mp4a.40.2", Bitrate: 128_000, Channels: 2, StreamIndex: 2, Language: "!!bogus!!"},
	}
	out := buildMaster(t, []domain.VideoRendition{video1080()}, audios, nil)
	parsed := assertMasterConforms(t, out)

	members := parsed.audio["audio-mp4a-40-2-2"]
	if len(members) != 3 {
		t.Fatalf("expected 3 members, got %d\n%s", len(members), out)
	}
	want := []struct{ name, language string }{
		{"Commentary 'cut'", "en"},
		{"Commentary 'cut' 2", "ja"},
		{"Audio 3", ""},
	}
	for index, expected := range want {
		if members[index].Name != expected.name || members[index].Language != expected.language {
			t.Fatalf("member %d = %q %q, want %q %q\n%s", index, members[index].Name, members[index].Language, expected.name, expected.language, out)
		}
	}
	if strings.Contains(out, "!!bogus!!") || strings.Contains(out, "LANGUAGE=\"\"") {
		t.Fatalf("unparseable language leaked into the playlist\n%s", out)
	}
}

func TestMasterSubtitleLanguageIsRFC5646(t *testing.T) {
	subtitles := []domain.SubtitleStream{{ID: "a", Language: "fra"}, {ID: "b", Language: "zz-not-a-tag-"}}
	out := buildMaster(t, []domain.VideoRendition{video1080()}, nil, subtitles)
	if !strings.Contains(out, `NAME="fra",LANGUAGE="fr"`) {
		t.Fatalf("expected fra normalised to fr\n%s", out)
	}
	if strings.Contains(out, "not-a-tag") && strings.Contains(out, `LANGUAGE="zz`) {
		t.Fatalf("unparseable subtitle language leaked\n%s", out)
	}
}

func TestMasterBandwidthUsesPeakBandwidthFacts(t *testing.T) {
	video := video1080()
	video.PeakBandwidth = 11_000_000
	audios := twoTrackAudio()[:2]
	audios[0].PeakBandwidth = 220_000
	audios[1].PeakBandwidth = 290_000
	parsed := parseMaster(t, buildMaster(t, []domain.VideoRendition{video}, audios, nil))
	variant := variantByURI(t, parsed, "/media/video/1080p/playlist.m3u8", "audio-mp4a-40-2-2")
	if want := uint32(11_000_000 + 290_000); variant.Bandwidth != want {
		t.Fatalf("BANDWIDTH = %d, want %d", variant.Bandwidth, want)
	}
	if want := uint32(5_000_000 + 128_000); variant.AverageBandwidth != want {
		t.Fatalf("AVERAGE-BANDWIDTH = %d, want %d", variant.AverageBandwidth, want)
	}
}

func TestMasterBandwidthFallsBackWhenPeakBandwidthZero(t *testing.T) {
	parsed := parseMaster(t, buildMaster(t, []domain.VideoRendition{video1080()}, twoTrackAudio()[:2], nil))
	variant := variantByURI(t, parsed, "/media/video/1080p/playlist.m3u8", "audio-mp4a-40-2-2")
	if want := uint32(7_500_000 + 128_000); variant.Bandwidth != want {
		t.Fatalf("BANDWIDTH = %d, want %d", variant.Bandwidth, want)
	}
}

func TestMasterAudioOnlyBandwidthUsesPeakBandwidth(t *testing.T) {
	audios := twoTrackAudio()[:2]
	audios[0].PeakBandwidth = 220_000
	audios[1].PeakBandwidth = 290_000
	parsed := parseMaster(t, buildMaster(t, nil, audios, nil))
	if len(parsed.playlist.Variants) != 1 {
		t.Fatalf("variants = %d\n%s", len(parsed.playlist.Variants), parsed.raw)
	}
	variant := parsed.playlist.Variants[0]
	if variant.Bandwidth != 290_000 || variant.AverageBandwidth != 128_000 {
		t.Fatalf("BANDWIDTH=%d AVERAGE=%d\n%s", variant.Bandwidth, variant.AverageBandwidth, parsed.raw)
	}
}

func TestMasterAverageBandwidthUsesAverageFacts(t *testing.T) {
	video := video1080()
	video.AverageBandwidth = 4_200_000
	audios := twoTrackAudio()[:2]
	audios[0].AverageBandwidth = 100_000
	audios[1].AverageBandwidth = 120_000
	parsed := parseMaster(t, buildMaster(t, []domain.VideoRendition{video}, audios, nil))
	variant := variantByURI(t, parsed, "/media/video/1080p/playlist.m3u8", "audio-mp4a-40-2-2")
	if want := uint32(4_200_000 + 120_000); variant.AverageBandwidth != want {
		t.Fatalf("AVERAGE-BANDWIDTH = %d, want %d", variant.AverageBandwidth, want)
	}
}

func TestMasterAudioOnlyAverageBandwidthUsesAverageFacts(t *testing.T) {
	audios := twoTrackAudio()[:2]
	audios[0].AverageBandwidth = 100_000
	audios[1].AverageBandwidth = 120_000
	parsed := parseMaster(t, buildMaster(t, nil, audios, nil))
	if variant := parsed.playlist.Variants[0]; variant.AverageBandwidth != 120_000 {
		t.Fatalf("AVERAGE=%d\n%s", variant.AverageBandwidth, parsed.raw)
	}
}
