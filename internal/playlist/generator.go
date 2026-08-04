package playlist

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/eleven-am/goshl/internal/domain"
)

type Generator struct {
	pathGen domain.PathGenerator
}

func NewGenerator(pathGen domain.PathGenerator) *Generator {
	return &Generator{pathGen: pathGen}
}

func (g *Generator) Master(sourceURL string, videos []domain.VideoRendition, audios []domain.AudioRendition, subtitles []domain.SubtitleStream) string {
	var b strings.Builder

	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:4\n")
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	b.WriteString("\n")

	groups := groupAudioRenditions(audios)
	for _, group := range groups {
		for index, audio := range group.Audios {
			language := audio.Language
			if language == "" {
				language = "und"
			}
			b.WriteString(fmt.Sprintf(
				"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"%s\",NAME=\"%s\",LANGUAGE=\"%s\",CHANNELS=\"%d\",DEFAULT=%s,AUTOSELECT=YES,URI=\"%s\"\n",
				escapeAttribute(group.ID),
				escapeAttribute(audio.Name),
				escapeAttribute(language),
				audio.Channels,
				defaultFlag(audio.Default || index == 0),
				escapeURI(g.pathGen.VariantPlaylist(sourceURL, audio.Name, domain.StreamAudio)),
			))
		}
	}

	if len(audios) > 0 {
		b.WriteString("\n")
	}
	for index, subtitle := range subtitles {
		name := subtitle.Title
		if name == "" {
			name = subtitle.Language
		}
		if name == "" {
			name = subtitle.ID
		}
		language := subtitle.Language
		if language == "" {
			language = "und"
		}
		b.WriteString(fmt.Sprintf(
			"#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"subtitles\",NAME=\"%s\",LANGUAGE=\"%s\",DEFAULT=%s,AUTOSELECT=YES,FORCED=%s,URI=\"%s\"\n",
			escapeAttribute(name),
			escapeAttribute(language),
			defaultFlag(subtitle.Default || index == 0),
			defaultFlag(subtitle.Forced),
			escapeURI(g.pathGen.SubtitlePlaylist(sourceURL, subtitle.ID)),
		))
	}
	if len(subtitles) > 0 {
		b.WriteString("\n")
	}

	for _, video := range videos {
		if len(groups) == 0 {
			writeVideoVariant(&b, g.pathGen, sourceURL, video, audioGroup{}, len(subtitles) > 0)
			continue
		}
		for _, group := range groups {
			writeVideoVariant(&b, g.pathGen, sourceURL, video, group, len(subtitles) > 0)
		}
	}

	if len(videos) == 0 {
		for _, group := range groups {
			for _, audio := range group.Audios {
				bandwidth := audio.Bitrate
				b.WriteString(fmt.Sprintf("#EXT-X-STREAM-INF:AVERAGE-BANDWIDTH=%d,BANDWIDTH=%d,CODECS=\"%s\"\n", bandwidth, bandwidth, escapeAttribute(audio.CodecString)))
				b.WriteString(g.pathGen.VariantPlaylist(sourceURL, audio.Name, domain.StreamAudio) + "\n")
			}
		}
	}

	return b.String()
}

type audioGroup struct {
	ID           string
	Codec        string
	MaxBandwidth int
	Audios       []domain.AudioRendition
}

func groupAudioRenditions(audios []domain.AudioRendition) []audioGroup {
	byID := make(map[string]*audioGroup)
	var order []string
	for _, audio := range audios {
		codec := audio.CodecString
		if codec == "" {
			codec = audioCodecStringForCodec(audio.Codec)
		}
		id := "audio-" + sanitizeID(codec) + "-" + strconv.Itoa(audio.Channels)
		group := byID[id]
		if group == nil {
			group = &audioGroup{ID: id, Codec: codec}
			byID[id] = group
			order = append(order, id)
		}
		audio.CodecString = codec
		audio.GroupID = id
		group.Audios = append(group.Audios, audio)
		if audio.Bitrate > group.MaxBandwidth {
			group.MaxBandwidth = audio.Bitrate
		}
	}
	groups := make([]audioGroup, 0, len(order))
	for _, id := range order {
		groups = append(groups, *byID[id])
	}
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].ID < groups[j].ID })
	return groups
}

func writeVideoVariant(b *strings.Builder, pathGen domain.PathGenerator, sourceURL string, video domain.VideoRendition, group audioGroup, hasSubtitles bool) {
	average := video.Bitrate + group.MaxBandwidth
	peak := video.PeakBitrate
	if peak <= 0 {
		peak = video.Bitrate
	}
	peak += group.MaxBandwidth
	codec := video.Codec
	if codec == "" {
		codec = videoCodecString(video)
	}
	codecs := codec
	attrs := ""
	if group.ID != "" {
		codecs += "," + group.Codec
		attrs = fmt.Sprintf(",AUDIO=\"%s\"", escapeAttribute(group.ID))
	}
	if hasSubtitles {
		attrs += ",SUBTITLES=\"subtitles\""
	}
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
		attrs,
	))
	b.WriteString(pathGen.VariantPlaylist(sourceURL, video.Name, domain.StreamVideo) + "\n")
}

func (g *Generator) Subtitle(sourceURL, trackID string, duration float64) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:4\n")
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", int(math.Ceil(duration))))
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n\n")
	b.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", duration))
	b.WriteString(g.pathGen.SubtitleVTT(sourceURL, trackID) + "\n")
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
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

func (g *Generator) Variant(sourceURL string, rendition string, streamType domain.StreamType, segments []domain.Segment) string {
	var b strings.Builder

	var maxDuration float64
	for _, seg := range segments {
		if seg.Duration > maxDuration {
			maxDuration = seg.Duration
		}
	}

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

	return b.String()
}

func defaultFlag(isDefault bool) string {
	if isDefault {
		return "YES"
	}
	return "NO"
}

func videoCodecString(video domain.VideoRendition) string {
	level := video.Level
	if level <= 0 {
		switch {
		case video.Height >= 2160 && video.FrameRate > 30:
			level = 52
		case video.Height >= 2160:
			level = 51
		case video.Height >= 1080 && video.FrameRate > 30:
			level = 42
		case video.Height >= 1080:
			level = 40
		case video.Height >= 720 && video.FrameRate > 30:
			level = 32
		case video.Height >= 720:
			level = 31
		case video.Height >= 480:
			level = 30
		default:
			level = 21
		}
	}
	return fmt.Sprintf("avc1.6400%02x", level)
}

func audioCodecStringForCodec(codec string) string {
	switch codec {
	case "ac3":
		return "ac-3"
	case "eac3":
		return "ec-3"
	default:
		return "mp4a.40.2"
	}
}
