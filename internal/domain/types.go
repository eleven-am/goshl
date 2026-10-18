package domain

import "time"

type Job struct {
	ID              string
	RunID           string
	MediaKey        string
	Profile         string
	SourceURL       string
	Rendition       string
	StreamType      StreamType
	StartIndex      int
	EndIndex        int
	RequestedIndex  int
	EnqueuedAt      time.Time
	Deadline        time.Time
	Segments        []Segment
	Overlap         *Segment
	ExpectedIndices []int
	VideoRendition  *VideoRendition
	AudioRendition  *AudioRendition
}

type JobDelivery struct {
	Job     Job
	Receipt string
}

type JobState string

const (
	JobStateCompleted JobState = "completed"
	JobStateFailed    JobState = "failed"
	JobStateCancelled JobState = "cancelled"
)

type JobStatus struct {
	JobID string
	RunID string
	State JobState
	Error string
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
	StartTime float64
	Duration  float64
	Keyframes []float64
	Video     VideoStream
	Audios    []AudioStream
	Subtitles []SubtitleStream
}

type VideoStream struct {
	Index              int
	Codec              string
	Profile            string
	Level              int
	PixelFormat        string
	BitDepth           int
	Width              int
	Height             int
	Bitrate            int
	FrameRate          float64
	ColorRange         string
	ColorSpace         string
	ColorTransfer      string
	ColorPrimaries     string
	SampleAspect       string
	Rotation           int
	Default            bool
	StartsWithKeyframe bool
}

type AudioStream struct {
	Index         int
	Codec         string
	Language      string
	Title         string
	Channels      int
	ChannelLayout string
	Bitrate       int
	Default       bool
}

type SubtitleStream struct {
	Index    int
	ID       string
	Codec    string
	Language string
	Title    string
	Forced   bool
	Default  bool
}

type PreviewConfig struct {
	MaxFrames     int
	Frameduration int
	Width         int
}

type ThumbnailConfig struct {
	Position float64
	Width    int
}
