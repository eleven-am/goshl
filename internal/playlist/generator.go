package playlist

import (
	"fmt"
	"math"
	"strings"

	"github.com/eleven-am/goshl/internal/domain"
)

type Generator struct {
	pathGen domain.PathGenerator
}

func NewGenerator(pathGen domain.PathGenerator) *Generator {
	return &Generator{pathGen: pathGen}
}

func (g *Generator) Master(sourceURL string, videos []domain.VideoRendition, audios []domain.AudioRendition) string {
	var b strings.Builder

	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:4\n")
	b.WriteString("\n")

	audioGroupID := "audio"
	for _, audio := range audios {
		b.WriteString(fmt.Sprintf(
			"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"%s\",NAME=\"%s\",DEFAULT=%s,AUTOSELECT=YES,URI=\"%s\"\n",
			audioGroupID,
			audio.Name,
			defaultFlag(audio.Name == "aac_stereo"),
			g.pathGen.VariantPlaylist(sourceURL, audio.Name, domain.StreamAudio),
		))
	}

	if len(audios) > 0 {
		b.WriteString("\n")
	}

	for _, video := range videos {
		codecs := fmt.Sprintf("%s,%s", videoCodecString(video), audioCodecString())
		streamInf := fmt.Sprintf(
			"#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%dx%d,CODECS=\"%s\",AUDIO=\"%s\"",
			video.Bitrate,
			video.Width,
			video.Height,
			codecs,
			audioGroupID,
		)
		b.WriteString(streamInf + "\n")
		b.WriteString(g.pathGen.VariantPlaylist(sourceURL, video.Name, domain.StreamVideo) + "\n")
	}

	return b.String()
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
	switch video.Height {
	case 2160:
		return "avc1.640033"
	case 1080:
		return "avc1.640028"
	case 720:
		return "avc1.64001f"
	case 480:
		return "avc1.64001e"
	default:
		return "avc1.640015"
	}
}

func audioCodecString() string {
	return "mp4a.40.2"
}
