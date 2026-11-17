package live

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	ErrSourceUnavailable = errors.New("live source unavailable")
	ErrUnsupportedCodec  = errors.New("live source codec is not supported")
	ErrNoData            = errors.New("live source produced no usable data")
	ErrCapacity          = errors.New("live capacity reached")
	ErrStopped           = errors.New("live session stopped")
	ErrSegmentNotFound   = errors.New("live segment not found")
	ErrInvalidOptions    = errors.New("invalid live options")
)

type Source struct {
	Open  func(ctx context.Context) (io.ReadCloser, error)
	Label string
}

type Options struct {
	SegmentDuration     time.Duration
	WindowSegments      int
	IdleTimeout         time.Duration
	MaxHeight           int
	PreferredLanguage   string
	ReadySegments       int
	TranscodeInterlaced bool
}

func (o *Options) setDefaults() {
	if o.SegmentDuration == 0 {
		o.SegmentDuration = 4 * time.Second
	}
	if o.WindowSegments == 0 {
		o.WindowSegments = 6
	}
	if o.IdleTimeout == 0 {
		o.IdleTimeout = 60 * time.Second
	}
	if o.ReadySegments == 0 {
		o.ReadySegments = 3
	}
	if o.MaxHeight == 0 {
		o.MaxHeight = 720
	}
}

func (o Options) validate() error {
	if o.SegmentDuration < time.Second || o.SegmentDuration > 30*time.Second || o.WindowSegments < 3 || o.WindowSegments > 100 || o.IdleTimeout < time.Second || o.MaxHeight < 144 || o.ReadySegments < 1 || o.ReadySegments > o.WindowSegments {
		return ErrInvalidOptions
	}
	return nil
}

type Status string

const (
	StatusStarting Status = "starting"
	StatusRunning  Status = "running"
	StatusFailed   Status = "failed"
	StatusStopped  Status = "stopped"
)

type State struct {
	Status Status
	Err    error
}

type Action string

const (
	ActionCopy      Action = "copy"
	ActionTranscode Action = "transcode"
	ActionNone      Action = "none"
)

type VideoInfo struct {
	PID         int
	Codec       string
	Profile     string
	PixelFormat string
	Width       int
	Height      int
	DisplayW    int
	FrameRate   float64
	Interlaced  bool
	Transfer    string
	Primaries   string
	Matrix      string
}

type AudioInfo struct {
	PID        int
	Codec      string
	Language   string
	Channels   int
	SampleRate int
}

type ProbeInfo struct {
	Video         VideoInfo
	Audios        []AudioInfo
	SelectedAudio int
	VideoAction   Action
	AudioAction   Action
	Programs      int
	ProbedBytes   int
}
