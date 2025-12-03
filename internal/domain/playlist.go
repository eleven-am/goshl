package domain

type Segment struct {
	Index    int
	Start    float64
	End      float64
	Duration float64
}

type SegmentData struct {
	SourceURL string
	Index     int
	StartPTS  uint64
	EndPTS    uint64
	Duration  float64
	Rendition string
	IsVideo   bool
}

type MasterPlaylist struct {
	Videos []VideoRendition
	Audios []AudioRendition
}

type VariantPlaylist struct {
	Rendition      string
	StreamType     StreamType
	TargetDuration float64
	Segments       []Segment
}

type StreamType string

const (
	StreamVideo    StreamType = "video"
	StreamAudio    StreamType = "audio"
	StreamSubtitle StreamType = "subtitle"
)

type PathGenerator interface {
	MasterPlaylist(sourceURL string) string
	VariantPlaylist(sourceURL string, rendition string, streamType StreamType) string
	Segment(sourceURL string, rendition string, streamType StreamType, index int) string
	SpriteVTT(sourceURL string) string
	Sprite(sourceURL string, index int) string
	SubtitleVTT(sourceURL string, lang string) string
}
