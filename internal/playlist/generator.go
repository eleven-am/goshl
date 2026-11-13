package playlist

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/text/language"

	"github.com/eleven-am/goshl/internal/domain"
)

const (
	subtitleGroupID = "subtitles"
	sameCodecRank   = 1 << 16
)

var (
	ErrNoRenditions     = errors.New("playlist: no renditions")
	ErrInvalidRendition = errors.New("playlist: invalid rendition")
	ErrEmptyPlan        = errors.New("playlist: no segments")
)

type Generator struct {
	pathGen domain.PathGenerator
}

func NewGenerator(pathGen domain.PathGenerator) *Generator {
	return &Generator{pathGen: pathGen}
}

type audioTrack struct {
	StreamIndex int
	Name        string
	Title       string
	Label       string
	Language    string
	Default     bool
	Autoselect  bool
	Renditions  []domain.AudioRendition
}

type audioMember struct {
	Track     *audioTrack
	Rendition domain.AudioRendition
}

type audioGroup struct {
	ID               string
	Codec            string
	Channels         int
	Codecs           []string
	MaxBandwidth     int
	PeakBandwidth    int
	AverageBandwidth int
	Members          []audioMember
}

type subtitleMember struct {
	Stream     domain.SubtitleStream
	Name       string
	Language   string
	Default    bool
	Autoselect bool
}

func (g *Generator) Master(sourceURL string, videos []domain.VideoRendition, audios []domain.AudioRendition, subtitles []domain.SubtitleStream) (string, error) {
	if len(videos) == 0 && len(audios) == 0 {
		return "", ErrNoRenditions
	}
	for _, video := range videos {
		if video.Codec == "" {
			return "", fmt.Errorf("%w: video rendition %q has no codec string", ErrInvalidRendition, video.Name)
		}
		if video.Bitrate <= 0 {
			return "", fmt.Errorf("%w: video rendition %q has no bitrate", ErrInvalidRendition, video.Name)
		}
	}
	for _, audio := range audios {
		if audio.CodecString == "" {
			return "", fmt.Errorf("%w: audio rendition %q has no codec string", ErrInvalidRendition, audio.Name)
		}
		if audio.Bitrate <= 0 {
			return "", fmt.Errorf("%w: audio rendition %q has no bitrate", ErrInvalidRendition, audio.Name)
		}
	}

	tracks := audioTracks(audios)
	groups, err := audioGroups(tracks)
	if err != nil {
		return "", err
	}
	subs := subtitleMembers(subtitles)

	var b strings.Builder

	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:4\n")
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	b.WriteString("\n")

	for _, group := range groups {
		for _, member := range group.Members {
			b.WriteString(fmt.Sprintf(
				"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"%s\",NAME=\"%s\"%s,CHANNELS=\"%d\",DEFAULT=%s,AUTOSELECT=%s,URI=\"%s\"\n",
				escapeAttribute(group.ID),
				member.Track.Name,
				languageAttribute(member.Track.Language),
				member.Rendition.Channels,
				yesNo(member.Track.Default),
				yesNo(member.Track.Autoselect),
				escapeURI(g.pathGen.VariantPlaylist(sourceURL, member.Rendition.Name, domain.StreamAudio)),
			))
		}
	}
	if len(groups) > 0 {
		b.WriteString("\n")
	}

	for _, sub := range subs {
		b.WriteString(fmt.Sprintf(
			"#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"%s\",NAME=\"%s\"%s,DEFAULT=%s,AUTOSELECT=%s,FORCED=%s,URI=\"%s\"\n",
			subtitleGroupID,
			sub.Name,
			languageAttribute(sub.Language),
			yesNo(sub.Default),
			yesNo(sub.Autoselect),
			yesNo(sub.Stream.Forced),
			escapeURI(g.pathGen.SubtitlePlaylist(sourceURL, sub.Stream.ID)),
		))
	}
	if len(subs) > 0 {
		b.WriteString("\n")
	}

	for _, video := range videos {
		if len(groups) == 0 {
			writeVideoVariant(&b, g.pathGen, sourceURL, video, audioGroup{}, len(subs) > 0)
			continue
		}
		for _, group := range groups {
			writeVideoVariant(&b, g.pathGen, sourceURL, video, group, len(subs) > 0)
		}
	}

	if len(videos) == 0 {
		for _, group := range groups {
			writeAudioVariant(&b, g.pathGen, sourceURL, group, len(subs) > 0)
		}
	}

	return b.String(), nil
}

func audioTracks(audios []domain.AudioRendition) []*audioTrack {
	byIndex := make(map[int]*audioTrack)
	var tracks []*audioTrack
	defaultIndex := -1
	for _, audio := range audios {
		track := byIndex[audio.StreamIndex]
		if track == nil {
			track = &audioTrack{StreamIndex: audio.StreamIndex, Language: normalizeLanguage(audio.Language), Label: escapeAttribute(audio.Language), Title: escapeAttribute(audio.Title)}
			byIndex[audio.StreamIndex] = track
			tracks = append(tracks, track)
		}
		track.Renditions = append(track.Renditions, audio)
		if audio.Default && defaultIndex < 0 {
			defaultIndex = audio.StreamIndex
		}
	}
	if len(tracks) == 0 {
		return nil
	}
	if defaultIndex < 0 {
		defaultIndex = tracks[0].StreamIndex
	}

	names := make(map[string]bool)
	languages := make(map[string]bool)
	byIndex[defaultIndex].Default = true
	byIndex[defaultIndex].Autoselect = true
	languages[byIndex[defaultIndex].Language] = true
	for position, track := range tracks {
		name := track.Title
		if name == "" && track.Language != "" {
			name = track.Label
		}
		if name == "" || name == "und" {
			name = "Audio " + strconv.Itoa(position+1)
		}
		track.Name = uniqueName(names, name)
		if !languages[track.Language] {
			languages[track.Language] = true
			track.Autoselect = true
		}
	}
	return tracks
}

func audioGroups(tracks []*audioTrack) ([]audioGroup, error) {
	byID := make(map[string]*audioGroup)
	var order []string
	for _, track := range tracks {
		for _, audio := range track.Renditions {
			id := "audio-" + sanitizeID(audio.CodecString) + "-" + strconv.Itoa(audio.Channels)
			if byID[id] == nil {
				byID[id] = &audioGroup{ID: id, Codec: audio.CodecString, Channels: audio.Channels}
				order = append(order, id)
			}
		}
	}
	sort.Strings(order)

	groups := make([]audioGroup, 0, len(order))
	for _, id := range order {
		group := byID[id]
		for _, track := range tracks {
			rendition, err := groupRendition(*group, track)
			if err != nil {
				return nil, err
			}
			group.Members = append(group.Members, audioMember{Track: track, Rendition: rendition})
			if rendition.Bitrate > group.MaxBandwidth {
				group.MaxBandwidth = rendition.Bitrate
			}
			if average := audioAverage(rendition); average > group.AverageBandwidth {
				group.AverageBandwidth = average
			}
			if peak := audioPeak(rendition); peak > group.PeakBandwidth {
				group.PeakBandwidth = peak
			}
		}
		group.Codecs = []string{group.Codec}
		for _, member := range group.Members {
			if !containsString(group.Codecs, member.Rendition.CodecString) {
				group.Codecs = append(group.Codecs, member.Rendition.CodecString)
			}
		}
		groups = append(groups, *group)
	}
	return groups, nil
}

func audioAverage(audio domain.AudioRendition) int {
	if audio.AverageBandwidth > 0 {
		return audio.AverageBandwidth
	}
	return audio.Bitrate
}

func audioPeak(audio domain.AudioRendition) int {
	if audio.PeakBandwidth > 0 {
		return audio.PeakBandwidth
	}
	return audio.Bitrate
}

func groupRendition(group audioGroup, track *audioTrack) (domain.AudioRendition, error) {
	var native []domain.AudioRendition
	for _, audio := range track.Renditions {
		if audio.CodecString == group.Codec && audio.Channels == group.Channels {
			native = append(native, audio)
		}
	}
	if len(native) > 1 {
		return domain.AudioRendition{}, fmt.Errorf("%w: audio renditions %q and %q of stream %d share group %s", ErrInvalidRendition, native[0].Name, native[1].Name, track.StreamIndex, group.ID)
	}
	if len(native) == 1 {
		return native[0], nil
	}

	best := -1
	for index, audio := range track.Renditions {
		if best < 0 || fallbackRank(group, audio) > fallbackRank(group, track.Renditions[best]) {
			best = index
		}
	}
	return track.Renditions[best], nil
}

func fallbackRank(group audioGroup, audio domain.AudioRendition) int {
	if audio.Channels > group.Channels {
		return 0
	}
	rank := 1 + audio.Channels
	if audio.CodecString == group.Codec {
		rank += sameCodecRank
	}
	return rank
}

func subtitleMembers(subtitles []domain.SubtitleStream) []subtitleMember {
	members := make([]subtitleMember, 0, len(subtitles))
	names := make(map[string]bool)
	defaultIndex := -1
	for index, subtitle := range subtitles {
		name := escapeAttribute(subtitle.Title)
		if name == "" {
			name = escapeAttribute(subtitle.Language)
		}
		if name == "" {
			name = escapeAttribute(subtitle.ID)
		}
		if name == "" {
			name = "Subtitle " + strconv.Itoa(index+1)
		}
		language := normalizeLanguage(subtitle.Language)
		if subtitle.Default && defaultIndex < 0 {
			defaultIndex = index
		}
		members = append(members, subtitleMember{Stream: subtitle, Name: uniqueName(names, name), Language: language})
	}

	selected := make(map[string]bool)
	if defaultIndex >= 0 {
		members[defaultIndex].Default = true
		members[defaultIndex].Autoselect = true
		selected[subtitleSelectionKey(members[defaultIndex])] = true
	}
	for index := range members {
		key := subtitleSelectionKey(members[index])
		if members[index].Stream.Forced || !selected[key] {
			selected[key] = true
			members[index].Autoselect = true
		}
	}
	return members
}

func subtitleSelectionKey(member subtitleMember) string {
	return member.Language + "\x00" + yesNo(member.Stream.Forced)
}

func normalizeLanguage(code string) string {
	code = strings.TrimSpace(code)
	if code == "" {
		return ""
	}
	tag, err := language.Parse(code)
	if err != nil {
		return ""
	}
	return tag.String()
}

func languageAttribute(code string) string {
	if code == "" {
		return ""
	}
	return fmt.Sprintf(",LANGUAGE=\"%s\"", escapeAttribute(code))
}

func uniqueName(used map[string]bool, name string) string {
	candidate := name
	for suffix := 2; used[candidate]; suffix++ {
		candidate = name + " " + strconv.Itoa(suffix)
	}
	used[candidate] = true
	return candidate
}

func containsString(values []string, value string) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}

func variantAttributes(group audioGroup, hasSubtitles bool) string {
	attrs := ""
	if group.ID != "" {
		attrs = fmt.Sprintf(",AUDIO=\"%s\"", escapeAttribute(group.ID))
	}
	if hasSubtitles {
		attrs += fmt.Sprintf(",SUBTITLES=\"%s\"", subtitleGroupID)
	}
	return attrs
}

func writeVideoVariant(b *strings.Builder, pathGen domain.PathGenerator, sourceURL string, video domain.VideoRendition, group audioGroup, hasSubtitles bool) {
	average := video.AverageBandwidth
	if average <= 0 {
		average = video.Bitrate
	}
	average += group.AverageBandwidth
	peak := video.PeakBandwidth
	if peak <= 0 {
		peak = video.PeakBitrate
	}
	if peak <= 0 {
		peak = video.Bitrate
	}
	peak += group.PeakBandwidth
	codecs := strings.Join(append([]string{video.Codec}, group.Codecs...), ",")
	frameRate := ""
	if video.FrameRate > 0 {
		frameRate = fmt.Sprintf(",FRAME-RATE=%.3f", video.FrameRate)
	}
	b.WriteString(fmt.Sprintf(
		"#EXT-X-STREAM-INF:AVERAGE-BANDWIDTH=%d,BANDWIDTH=%d,RESOLUTION=%dx%d,CODECS=\"%s\"%s%s\n",
		average,
		peak,
		video.Width,
		video.Height,
		escapeAttribute(codecs),
		frameRate,
		variantAttributes(group, hasSubtitles),
	))
	b.WriteString(pathGen.VariantPlaylist(sourceURL, video.Name, domain.StreamVideo) + "\n")
}

func writeAudioVariant(b *strings.Builder, pathGen domain.PathGenerator, sourceURL string, group audioGroup, hasSubtitles bool) {
	primary := group.Members[0]
	for _, member := range group.Members {
		if member.Track.Default {
			primary = member
		}
	}
	b.WriteString(fmt.Sprintf(
		"#EXT-X-STREAM-INF:AVERAGE-BANDWIDTH=%d,BANDWIDTH=%d,CODECS=\"%s\"%s\n",
		group.AverageBandwidth,
		group.PeakBandwidth,
		escapeAttribute(strings.Join(group.Codecs, ",")),
		variantAttributes(group, hasSubtitles),
	))
	b.WriteString(pathGen.VariantPlaylist(sourceURL, primary.Rendition.Name, domain.StreamAudio) + "\n")
}

func (g *Generator) Subtitle(sourceURL, trackID string, duration float64) (string, error) {
	if !(duration > 0) || math.IsInf(duration, 0) {
		return "", fmt.Errorf("%w: subtitle duration %v", ErrEmptyPlan, duration)
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:4\n")
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", int(math.Ceil(duration))))
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n\n")
	b.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", duration))
	b.WriteString(g.pathGen.SubtitleVTT(sourceURL, trackID) + "\n")
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String(), nil
}

func sanitizeID(value string) string {
	value = strings.ToLower(value)
	return strings.NewReplacer(".", "-", "+", "-", " ", "-").Replace(value)
}

func escapeAttribute(value string) string {
	return strings.NewReplacer("\\", "", "\"", "'", "\r", "", "\n", "").Replace(value)
}

func escapeURI(value string) string {
	return strings.NewReplacer("\"", "%22", "\r", "", "\n", "").Replace(value)
}

func (g *Generator) Variant(sourceURL string, rendition string, streamType domain.StreamType, segments []domain.Segment) (string, error) {
	if len(segments) == 0 {
		return "", ErrEmptyPlan
	}

	var maxDuration float64
	for _, seg := range segments {
		if !(seg.Duration > 0) || math.IsInf(seg.Duration, 0) {
			return "", fmt.Errorf("%w: segment %d has duration %v", ErrEmptyPlan, seg.Index, seg.Duration)
		}
		if seg.Duration > maxDuration {
			maxDuration = seg.Duration
		}
	}

	var b strings.Builder

	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:4\n")
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", int(math.Ceil(maxDuration))))
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	b.WriteString("\n")

	for _, seg := range segments {
		b.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", seg.Duration))
		b.WriteString(g.pathGen.Segment(sourceURL, rendition, streamType, seg.Index) + "\n")
	}

	b.WriteString("#EXT-X-ENDLIST\n")

	return b.String(), nil
}

func yesNo(value bool) string {
	if value {
		return "YES"
	}
	return "NO"
}
