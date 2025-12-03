package domain

type Job struct {
	ID         string
	SourceURL  string
	Rendition  string
	StreamType StreamType
	StartIndex int
	EndIndex   int
}

type SegmentState int

const (
	SegmentStateReady SegmentState = iota
	SegmentStateError
)

type SegmentStatus struct {
	State SegmentState
	Error string
}

type Metadata struct {
	Duration  float64
	Keyframes []float64
	Video     VideoStream
	Audios    []AudioStream
	Subtitles []SubtitleStream
}

type VideoStream struct {
	Index     int
	Codec     string
	Width     int
	Height    int
	Bitrate   int
	FrameRate float64
}

type AudioStream struct {
	Index    int
	Codec    string
	Language string
	Channels int
	Bitrate  int
}

type SubtitleStream struct {
	Index    int
	Codec    string
	Language string
	Forced   bool
}
