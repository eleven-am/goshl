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
- Extracts subtitles to WebVTT only when a client first requests a track

Segments are only transcoded when a client requests them. A 2-hour video doesn't need to finish transcoding before playback can start.

## Install

```bash
go get github.com/eleven-am/goshl
```

Requires `ffmpeg` and `ffprobe` in PATH.

## Quick start

```go
controller, err := goshl.NewControllerWithContext(ctx, goshl.Options{
    Storage:     myStorage,
    Coordinator: myCoordinator,
    PathGen:     myPathGen,
})
if err != nil {
    log.Fatal(err)
}

if err := controller.Start(ctx); err != nil {
    log.Fatal(err)
}
defer controller.Stop()
```

## Interfaces

You need to implement three interfaces:

### Storage

Handles persistence of segments, metadata, sprites, and subtitles.

Metadata methods receive an opaque media key. Asset methods receive an opaque asset key. Store these values exactly as provided; do not derive them again from the source URL. `SegmentData.MediaKey` carries the same opaque media identity. Goshl versions these keys using the source revision and active media pipeline so incompatible output is not reused.

```go
type Storage interface {
    MetadataExists(ctx context.Context, mediaKey string) (bool, error)
    GetMetadata(ctx context.Context, mediaKey string) ([]byte, error)
    SetMetadata(ctx context.Context, mediaKey string, data []byte) error

    WriteSegment(ctx context.Context, info SegmentData, data []byte) error
    ReadSegment(ctx context.Context, info SegmentData) ([]byte, error)
    SegmentExists(ctx context.Context, info SegmentData) (bool, error)

    WriteSprite(ctx context.Context, assetKey string, index int, data []byte) error
    ReadSprite(ctx context.Context, assetKey string, index int) ([]byte, error)
    SpriteExists(ctx context.Context, assetKey string, index int) (bool, error)

    WriteSpriteVTT(ctx context.Context, assetKey string, data []byte) error
    ReadSpriteVTT(ctx context.Context, assetKey string) ([]byte, error)
    SpriteVTTExists(ctx context.Context, assetKey string) (bool, error)

    WriteSubtitleVTT(ctx context.Context, assetKey string, trackID string, data []byte) error
    ReadSubtitleVTT(ctx context.Context, assetKey string, trackID string) ([]byte, error)
    SubtitleVTTExists(ctx context.Context, assetKey string, trackID string) (bool, error)

    WritePreview(ctx context.Context, assetKey string, data []byte) error
    ReadPreview(ctx context.Context, assetKey string) ([]byte, error)
    PreviewExists(ctx context.Context, assetKey string) (bool, error)

    WriteThumbnail(ctx context.Context, assetKey string, data []byte) error
    ReadThumbnail(ctx context.Context, assetKey string) ([]byte, error)
    ThumbnailExists(ctx context.Context, assetKey string) (bool, error)
}
```

### Coordinator

Manages job distribution and segment notifications. For single-instance deployments, an in-memory implementation works. For distributed setups, use something like Redis.

Jobs are concrete, versioned work contracts. A coordinator must preserve every `Job` field unchanged. Enqueue is idempotent by `Job.RunID`. Each delivery has an opaque receipt so settlement cannot affect another execution of the same deterministic batch.

```go
type Coordinator interface {
    Enqueue(ctx context.Context, job Job) error
    Subscribe(ctx context.Context, streamType StreamType, profile string) (<-chan JobDelivery, error)
    Renew(ctx context.Context, receipt string, ttl time.Duration) error
    Ack(ctx context.Context, receipt string) error

    SettleJob(ctx context.Context, receipt string, status JobStatus) error
    WaitJob(ctx context.Context, runID string) (<-chan JobStatus, error)

    NotifySegment(ctx context.Context, info SegmentData, status SegmentStatus) error
    WaitSegment(ctx context.Context, info SegmentData) (<-chan SegmentStatus, error)

    AcquireJob(ctx context.Context, jobID string, runID string, ttl time.Duration) (token string, activeRunID string, err error)
    ReleaseJob(ctx context.Context, jobID string, runID string, token string) error
    AcquireLock(ctx context.Context, key string, ttl time.Duration) (token string, err error)
    ReleaseLock(ctx context.Context, key string, token string) error
}
```

`Subscribe` is a competing-consumer subscription scoped to both stream type and pipeline profile. A job must only be delivered to a worker subscribed to its exact `Job.Profile`. Every delivery needs a unique receipt and an initial lease of at least five seconds. `Renew` extends that exact delivery, `Ack` discards only that receipt, and an unacknowledged delivery becomes available again after its lease expires. Redelivery invalidates the previous receipt so its worker can no longer renew or settle the delivery.

`Job.ID` is the deterministic batch identity used for locking. `Job.RunID` identifies one execution of that batch and is the queue idempotency key. Job status is keyed by `RunID`, must fan out to all current waiters, and the latest status must be replayed to a late waiter.

`AcquireJob` atomically acquires the deterministic batch lock and registers its run ID. When the lock is already held, it returns `ErrLockHeld` together with the active run ID. This lets every replica subscribe to the same durable job lifecycle. `ReleaseJob` must compare the job ID, run ID, and token before clearing the lock or active run.

`SettleJob` must atomically validate the active receipt, persist its terminal status, and remove the delivery. It must be idempotent when retried with the same receipt and status. The receipt-to-status settlement record must remain available until at least one minute after `Job.Deadline`. This prevents a crash between status publication and acknowledgement from redelivering terminal work. `Ack` is reserved for malformed deliveries that cannot supply a valid run status.

Job status must remain replayable until at least one minute after `Job.Deadline`. Segment status is only a transient readiness hint and must not replay notifications. Failures travel through run scoped job status. `WaitJob` and `WaitSegment` must unregister their subscribers when the supplied context ends. Goshl does not cancel a cluster job merely because one replica has no local waiters; another replica may still need it.

Terminal job states are immutable. A coordinator must not replace a terminal state.

### PathGenerator

Generates URLs that get embedded in playlists. These URLs should route back to your HTTP handlers.

```go
type PathGenerator interface {
    VariantPlaylist(sourceURL string, rendition string, streamType StreamType) string
    Segment(sourceURL string, rendition string, streamType StreamType, index int) string
    Sprite(sourceURL string, index int) string
    SubtitlePlaylist(sourceURL string, trackID string) string
    SubtitleVTT(sourceURL string, trackID string) string
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

// Returns a subtitle media playlist and WebVTT payload by stable track ID
playlist, err := controller.SubtitlePlaylist(ctx, sourceURL, "subtitle-3")
subs, err := controller.SubtitleVTT(ctx, sourceURL, "subtitle-3")
```

For progressive MP4 sources, Goshl reads the MP4 index and fetches only the bytes belonging to requested `mov_text`/`tx3g` or `wvtt` subtitle samples. Local files use random access and HTTP sources use validated byte-range requests. The generated WebVTT is stored through `Storage`, so later requests do not reopen the source.

Unsupported containers, fragmented MP4 layouts, subtitle codecs, and HTTP servers without byte-range support continue through the FFmpeg fallback. Subtitle extraction remains request driven; probing and ingest do not extract subtitle payloads.

## Options

```go
goshl.Options{
    Storage:        myStorage,          // required
    Coordinator:    myCoordinator,      // required
    PathGen:        myPathGen,          // required

    HWAccel:        false,              // auto-detect GPU encoding if available
    HWAccelMode:    goshl.AccelNone,    // explicitly choose none, cuda, videotoolbox, vaapi, or qsv
    SegmentTimeout: 30 * time.Second,   // max wait for segment transcoding
    JobTimeout:     2 * time.Minute,    // hard deadline for queued FFmpeg work
    TargetDuration: 6.0,                // target segment duration in seconds
    SegmentsPerJob: 4,                  // segments per transcoding job
    VideoPoolSize:  2,                  // video transcoding workers
    AudioPoolSize:  4,                  // audio transcoding workers

    CacheVersion:   "deployment-v1",   // bump when host-side media policy changes
    SourceRevision: revisionResolver,   // ETag/version resolver for remote sources
    Observer:       observer,           // structured lifecycle events

    HWAccelDecodeMode:    goshl.DecodeSafe,
    AllowHardwareFallback: false,
}
```

Local files use size and modification time as their default source revision. Remote URLs are treated as immutable unless `SourceRevision` returns an ETag, version, or content fingerprint. Call `controller.MediaKey` to inspect the resolved opaque key. If storage also implements `DeleteMedia(context.Context, string) error`, `controller.Invalidate` purges that media key.

## Hardware acceleration

Set `HWAccel: true` to auto-detect GPU encoding. Supported accelerators are NVIDIA CUDA/NVENC, Apple VideoToolbox, VAAPI, and Intel QSV. Detection performs a bounded decode/filter/encode self-test. Auto mode falls back to software and exposes the reason through `controller.HWAccelConfig()` and observer events.

Set `HWAccelMode` when the caller already knows the desired accelerator:

```go
controller := goshl.NewController(goshl.Options{
    Storage:     myStorage,
    Coordinator: myCoordinator,
    PathGen:     myPathGen,
    HWAccelMode: goshl.AccelCUDA,
    HWAccelDecodeMode: goshl.DecodeSafe,
})
```

`DecodeSafe` is the CUDA default: NVDEC is used without retaining decoder frames in the CUDA surface pool, then scaling runs on host frames before NVENC. This is the compatible path for the Blackwell decoder failure identified in the lab. `DecodeDevice` opts into the full device-frame decode/scale/encode graph. Explicit hardware selection fails construction when its self-test fails unless `AllowHardwareFallback` is enabled.

```go
cfg := controller.HWAccelConfig()
log.Printf("accelerator=%s requested=%s available=%v error=%s", cfg.Accelerator, cfg.Requested, cfg.Available, cfg.Error)
```

## Observability

`Observer` receives typed events for metadata and segment cache hits/misses, lock waits and release failures, job enqueue/start/completion/failure, acknowledgement failures, notification failures, segment readiness, and hardware selection/fallback. Event identities are opaque hashes; raw signed source URLs are not emitted. Observer callbacks should return promptly.

FFmpeg and FFprobe stderr are bounded, and source URLs, credentials, query strings, and media paths are redacted from returned command errors.

## Compatibility notes

This version intentionally tightens the distributed contracts:

See [MIGRATION.md](MIGRATION.md) for the complete consumer and rollout guide.

- `PathGenerator` must implement `SubtitlePlaylist`.
- Unused route methods and `Coordinator.Close` were removed from the required interfaces; lifecycle ownership remains with the host.
- `SegmentData` now uses `MediaKey` instead of overloading `SourceURL`.
- Coordinators must serialize the expanded concrete `Job` fields.
- Storage keys are versioned opaque identities rather than source URLs.
- Direct H.264 remuxing is restricted to compatible 8-bit `yuv420p` Baseline/Main/High streams that start with a keyframe; other inputs transcode to High-profile `yuv420p` H.264.

## Author

Roy Ossai

## License

GPL-3.0
