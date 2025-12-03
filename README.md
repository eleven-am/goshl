# Goshl

*Pronounced "goshul"*

An HLS transcoding library in Go.

## What it does

goshl takes a video file and serves it as an HLS stream. It:

- Probes the source to get resolution, duration, audio tracks, and subtitles
- Generates master and variant playlists
- Transcodes segments on demand (not upfront)
- Caches segments after transcoding
- Generates multiple quality levels based on source resolution
- Extracts thumbnail sprites for seek previews
- Extracts subtitles to WebVTT

Segments are only transcoded when a client requests them. A 2-hour video doesn't need to finish transcoding before playback can start.

## Install

```bash
go get github.com/eleven-am/goshl
```

Requires `ffmpeg` and `ffprobe` in PATH.

## Quick start

```go
controller := goshl.NewController(goshl.Options{
    Storage:     myStorage,
    Coordinator: myCoordinator,
    PathGen:     myPathGen,
})

if err := controller.Start(ctx); err != nil {
    log.Fatal(err)
}
defer controller.Stop()
```

## Interfaces

You need to implement three interfaces:

### Storage

Handles persistence of segments, metadata, sprites, and subtitles.

```go
type Storage interface {
    MetadataExists(ctx context.Context, sourceURL string) (bool, error)
    GetMetadata(ctx context.Context, sourceURL string) ([]byte, error)
    SetMetadata(ctx context.Context, sourceURL string, data []byte) error

    WriteSegment(ctx context.Context, info SegmentData, data []byte) error
    ReadSegment(ctx context.Context, info SegmentData) ([]byte, error)
    SegmentExists(ctx context.Context, info SegmentData) (bool, error)

    WriteSprite(ctx context.Context, sourceURL string, index int, data []byte) error
    ReadSprite(ctx context.Context, sourceURL string, index int) ([]byte, error)
    SpriteExists(ctx context.Context, sourceURL string, index int) (bool, error)

    WriteSpriteVTT(ctx context.Context, sourceURL string, data []byte) error
    ReadSpriteVTT(ctx context.Context, sourceURL string) ([]byte, error)
    SpriteVTTExists(ctx context.Context, sourceURL string) (bool, error)

    WriteSubtitleVTT(ctx context.Context, sourceURL string, lang string, data []byte) error
    ReadSubtitleVTT(ctx context.Context, sourceURL string, lang string) ([]byte, error)
    SubtitleVTTExists(ctx context.Context, sourceURL string, lang string) (bool, error)
}
```

### Coordinator

Manages job distribution and segment notifications. For single-instance deployments, an in-memory implementation works. For distributed setups, use something like Redis.

```go
type Coordinator interface {
    Enqueue(ctx context.Context, job Job) error
    Subscribe(ctx context.Context, streamType StreamType) (<-chan Job, error)
    Ack(ctx context.Context, jobID string) error

    NotifySegment(ctx context.Context, info SegmentData, status SegmentStatus) error
    WaitSegment(ctx context.Context, info SegmentData) (<-chan SegmentStatus, error)

    Close()
}
```

### PathGenerator

Generates URLs that get embedded in playlists. These URLs should route back to your HTTP handlers.

```go
type PathGenerator interface {
    MasterPlaylist(sourceURL string) string
    VariantPlaylist(sourceURL string, rendition string, streamType StreamType) string
    Segment(sourceURL string, rendition string, streamType StreamType, index int) string
    SpriteVTT(sourceURL string) string
    Sprite(sourceURL string, index int) string
    SubtitleVTT(sourceURL string, lang string) string
}
```

## Controller methods

```go
// Returns master playlist with available renditions
playlist, err := controller.MasterPlaylist(ctx, "file:///path/to/video.mp4")

// Returns variant playlist for a specific rendition
playlist, err := controller.VariantPlaylist(ctx, sourceURL, goshl.StreamVideo, "720p")

// Returns segment data (transcodes on first request, cached after)
data, err := controller.Segment(ctx, sourceURL, goshl.StreamVideo, "720p", 0)

// Returns WebVTT file for thumbnail sprites
vtt, err := controller.SpriteVTT(ctx, sourceURL)

// Returns sprite sheet image
sprite, err := controller.Sprite(ctx, sourceURL, 0)

// Returns subtitles in WebVTT format
subs, err := controller.SubtitleVTT(ctx, sourceURL, "en")
```

## Options

```go
goshl.Options{
    Storage:        myStorage,          // required
    Coordinator:    myCoordinator,      // required
    PathGen:        myPathGen,          // required

    HWAccel:        false,              // use GPU encoding if available
    SegmentTimeout: 30 * time.Second,   // max wait for segment transcoding
    TargetDuration: 6.0,                // target segment duration in seconds
    SegmentsPerJob: 10,                 // segments per transcoding job
    VideoPoolSize:  2,                  // video transcoding workers
    AudioPoolSize:  4,                  // audio transcoding workers
}
```

## Hardware acceleration

Set `HWAccel: true` to use GPU encoding. Supports NVIDIA NVENC and Apple VideoToolbox. Falls back to software encoding if unavailable.

## Author

Roy Ossai

## License

GPL-3.0