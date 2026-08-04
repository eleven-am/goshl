package domain

type PlaybackMethod string

const (
	DirectPlay   PlaybackMethod = "direct_play"
	DirectStream PlaybackMethod = "direct_stream"
	Transcode    PlaybackMethod = "transcode"
)

type VideoRendition struct {
	Name        string
	Width       int
	Height      int
	Bitrate     int
	PeakBitrate int
	FrameRate   float64
	Codec       string
	Level       int
	Method      PlaybackMethod
}

type AudioRendition struct {
	Name        string
	Codec       string
	CodecString string
	GroupID     string
	Bitrate     int
	Channels    int
	Method      PlaybackMethod

	StreamIndex int
	Language    string
	Default     bool
}
