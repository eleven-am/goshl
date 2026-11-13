package domain

type PlaybackMethod string

const (
	DirectStream PlaybackMethod = "direct_stream"
	Transcode    PlaybackMethod = "transcode"
)

type VideoRendition struct {
	Name               string
	Width              int
	Height             int
	Bitrate            int
	PeakBitrate        int
	BufferSize         int
	PeakBandwidth      int
	AverageBandwidth   int
	FrameRate          float64
	Codec              string
	Level              int
	Method             PlaybackMethod
	Colour             ColourPlan
	StripParameterSets bool
	SourceEnd          float64
}

type AudioRendition struct {
	Name             string
	Codec            string
	CodecString      string
	Bitrate          int
	Channels         int
	SampleRate       int
	PeakBandwidth    int
	AverageBandwidth int
	SourceEnd        float64
	Method           PlaybackMethod

	StreamIndex int
	Language    string
	Title       string
	Default     bool
}
