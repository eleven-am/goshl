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

### Runtime requirements

- `ffmpeg` and `ffprobe` in `PATH`, version 6.1 or newer. Transcoded video relies on `-enc_time_base demux`, which older releases do not have. `NewControllerWithContext` runs both binaries at startup and returns an error wrapping `ErrInvalidOptions` if either is older or its version output is unrecognised. Development builds (`git-...`, `N-...`) are accepted.
- For HDR sources (PQ or HLG transfer), an `ffmpeg` built with the `zscale` (libzimg) and `tonemap` filters, for example Homebrew `ffmpeg-full` or `jellyfin-ffmpeg`. Use the same build on every replica. This is not checked at startup: it is detected on first use and cached. Without these filters, `MasterPlaylist` and the video `VariantPlaylist` of an HDR source return an error wrapping `ErrTonemapUnavailable`, and its HDR renditions fail explicitly. The check runs on the host serving the playlist, so every replica must use the same build. SDR sources do not need it. Whether a PQ or HLG source is HDR is decided by its colour primaries, never by its bit depth: with BT.2020 primaries, or with unspecified primaries (assumed BT.2020), it is tone mapped, including 8-bit sources; with BT.709, SMPTE 170M or BT.470 (BG or M) primaries it is treated as a mis-tagged SDR source, retagged BT.709 and encoded normally without these filters.
- Source schemes are limited to local paths, `file:`, `http:` and `https:`. Every FFmpeg and FFprobe invocation runs with the protocol whitelist `file,http,https,tcp,tls`; any other scheme (for example `rtmp://`, `concat:` or `data:`) is rejected before FFmpeg starts. An empty source, an unsupported scheme or a malformed URL returns an error wrapping `ErrUnsupportedSource`.
- Trust model: goshl treats `sourceURL` as trusted. The host owns URL policy; goshl does not restrict which hosts an http(s) source may point at, including loopback and private IP addresses. FFmpeg, FFprobe and goshl's own range reader fetch the URL as given. goshl's own range reader (used for MP4 and MKV indexing and indexed subtitle extraction) follows at most 5 redirects, only to http(s) URLs, and refuses an https-to-http downgrade. FFmpeg and FFprobe follow redirects themselves under their own limits, which goshl does not control. TLS certificate verification by FFmpeg depends on its TLS backend (SecureTransport, OpenSSL or GnuTLS) and is not enforced by goshl, so hosts should only pass trusted https sources. If source URLs can come from users, validate them in your application and limit outbound network access to the origins you intend. Inputs are restricted to real container formats (`-format_whitelist`). Playlist formats such as HLS or ffconcat are rejected, so a source cannot make FFmpeg open further URLs or files.
- A ready-made runtime base image with a matching ffmpeg is published as `ghcr.io/eleven-am/goshl-ffmpeg:<version>` (multi-arch amd64 and arm64, Ubuntu 24.04, glibc). See [ffmpeg base image](#ffmpeg-base-image).
- Generated segments sit on a timeline that starts at 10 seconds rather than 0. Subtitle WebVTT files carry `X-TIMESTAMP-MAP=MPEGTS:900000,LOCAL:00:00:00.000` so players keep cues aligned with the video. Sprite, preview and thumbnail output is unaffected.

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

`NewController(opts)` does the same but panics on error.

## Interfaces

You implement three interfaces. `SpriteStorage` is an optional fourth.

### Storage

Handles persistence of segments, metadata, sprites, subtitles, previews and thumbnails.

Metadata methods receive an opaque media key. Asset methods receive an opaque asset key. Store these values exactly as provided; do not derive them again from the source URL. `SegmentData.MediaKey` carries the same opaque media identity. Goshl versions these keys using the source revision and active media pipeline so incompatible output is not reused.

```go
type SegmentData struct {
    MediaKey  string
    Index     int
    Rendition string
    IsVideo   bool
}

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

The sprite VTT is never stored: `SpriteVTT` renders it on each request from `PathGenerator.Sprite`.

If the storage also implements `DeleteMedia(ctx context.Context, mediaKey string) error`, `controller.Invalidate` uses it to purge everything stored for a media key.

### SpriteStorage

Optional. When `Options.SpriteStorage` is set, sprite sheets are written there instead of `Storage` (for example to keep them on a CDN-backed store). It is the sprite subset of `Storage`:

```go
type SpriteStorage interface {
    WriteSprite(ctx context.Context, assetKey string, index int, data []byte) error
    ReadSprite(ctx context.Context, assetKey string, index int) ([]byte, error)
    SpriteExists(ctx context.Context, assetKey string, index int) (bool, error)
}
```

To let `Invalidate` clean it, implement `DeleteMedia(ctx, mediaKey) error` on it as well.

### Coordinator

Manages job distribution and segment notifications. For single-instance deployments, an in-memory implementation works. For distributed setups, use something like Redis.

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

#### Job

Jobs are concrete, versioned work contracts. A coordinator must carry every `Job` field unchanged, including fields it does not understand:

```go
type Job struct {
    ID               string
    RunID            string
    MediaKey         string
    Profile          string
    SourceURL        string
    Rendition        string
    StreamType       StreamType
    StartIndex       int
    EndIndex         int
    RequestedIndex   int
    EnqueuedAt       time.Time
    StartBy          time.Time
    Segments         []Segment
    Overlap          *Segment
    Trailing         *Segment
    ExpectedIndices  []int
    VideoRendition   *VideoRendition
    AudioRendition   *AudioRendition
    VideoStreamIndex int
    Digest           string
}

type JobDelivery struct {
    Job     Job
    Receipt string
}

type JobStatus struct {
    JobID string
    RunID string
    State JobState
    Error string
}
```

`JobState` is `JobStateCompleted`, `JobStateFailed` or `JobStateCancelled`; all three are terminal.

`StartBy` is the latest moment a worker may begin the job. A worker refuses a delivery whose `StartBy` has passed, and otherwise runs it with a full `Options.JobTimeout` counted from the start of execution. `Overlap` and `Trailing` are extra media ranges the worker needs so segments join without gaps: `Overlap` is the segment before the first one (set for audio jobs and copied-video jobs that do not start at segment 0), `Trailing` is one segment after the last one (copied video and audio jobs), produced and discarded so the last requested segment ends on the correct frame. `VideoStreamIndex` selects the source video stream. A coordinator never interprets them.

`Digest` is a SHA-256 over every other field of the job except `EnqueuedAt` and `StartBy`, set by the replica that builds the job. A worker recomputes it and refuses the delivery when it differs, so a coordinator that drops, rounds or rewrites any field gets a failed job with a `digest mismatch` error and an `EventJobDeliveryRejected` event instead of wrong media. The two times are excluded, so a coordinator may store them at any precision or in any zone.

The rendition structs inside a job are part of the contract too. Round-trip every field, including the ones added since earlier releases:

```go
type VideoRendition struct {
    Name, Width, Height, Bitrate, PeakBitrate, BufferSize, PeakBandwidth, FrameRate, Codec, Level, Method
    Colour    ColourPlan // Tonemap, AssumeMatrix, AssumePrimaries
    SourceEnd float64    // video end in seconds when it ends early, otherwise 0
}

type AudioRendition struct {
    Name, Codec, CodecString, Bitrate, Channels, PeakBandwidth, Method
    SampleRate  int
    SourceEnd   float64 // end of the source audio stream in seconds, 0 if unknown
    StreamIndex int
    Language, Title string
    Default     bool
}
```

The job contract version is 5; it is part of the pipeline profile, so replicas on a different contract never share a queue. [MIGRATION.md](MIGRATION.md) lists what a coordinator must carry.

`Job.ID` is the deterministic batch identity used for locking. `Job.RunID` identifies one execution of that batch and is the queue idempotency key.

#### Contract

- A delivered `Job` must equal the enqueued one field for field. Workers verify `Job.Digest` and refuse a job that changed in transit.
- `Enqueue` is idempotent by `Job.RunID`. Return an error wrapping `goshl.ErrEnqueueRejected` only when the job was definitely not persisted; goshl then frees the batch at once so the next request can retry. Any other error is treated as ambiguous and the batch stays reserved until its lock expires.
- `Subscribe` is a competing-consumer subscription scoped to both stream type and pipeline profile. A job is delivered only to a worker subscribed to its exact `Job.Profile`. Every delivery has a unique receipt and an initial lease of at least five seconds. `Renew` extends that exact delivery, `Ack` discards only that receipt, and an unacknowledged delivery becomes available again when its lease expires. Redelivery invalidates the previous receipt.
- `SettleJob` atomically validates the active receipt, persists the terminal status and removes the delivery. It is idempotent when retried with the same receipt and status. The receipt-to-status record and the job status must stay available until at least one minute after `Job.StartBy` plus `JobTimeout`. `Ack` is reserved for malformed deliveries that cannot supply a valid status.
- `WaitJob` is keyed by `RunID`, fans out to all current waiters and replays the latest status to a late waiter. Terminal states are immutable.
- `AcquireJob` atomically acquires the batch lock and registers the run ID. When the lock is already held it returns `ErrLockHeld` together with the active run ID, so every replica can wait on the same run. `ReleaseJob` compares job ID, run ID and token before clearing anything.
- Segment status (`NotifySegment`, `WaitSegment`) is a transient readiness hint and must not replay. Failures travel through run-scoped job status.
- `WaitJob` and `WaitSegment` must unregister their subscribers when the supplied context ends. Goshl does not cancel a cluster job merely because one replica has no local waiters.

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

## Controller

`sourceURL` is a local path, a `file:` URL, or an `http(s)` URL. Rendition names contain only letters, digits, `.`, `_` and `-`, at most 64 characters; they must not start with `.` or contain `..`.

### Construction and lifecycle

| Method | Description |
|---|---|
| `NewControllerWithContext(ctx, Options) (*Controller, error)` | Validates options, applies defaults, runs hardware detection and validation. Returns an error wrapping `ErrInvalidOptions` for bad options. |
| `NewController(Options) *Controller` | Same, panics on error. |
| `Start(ctx) error` | Starts the video and audio worker pools. Returns `ErrControllerStopped` if `Stop` has already run, including when it ran during `Start`. |
| `Stop()` | Idempotent. Cancels detached work (asset generation, metadata probes, flight watchers), waits up to 10 seconds for it, then releases the locks of flights that were never enqueued. A `Segment` call racing `Stop` returns `ErrControllerStopped`. Locks of jobs already enqueued are not released; they expire by their TTL (`StartBy` plus `JobTimeout` plus one minute). A stopped controller cannot be restarted. |
| `HWAccelConfig() HWAccelConfig` | Copy of the selected hardware configuration: `Accelerator`, `Requested`, `Available`, `Error`, `DecodeMode`, plus the FFmpeg flags in use. |

After `Stop`, every method returns `ErrControllerStopped` (checked before argument validation) except `Stop` itself, `HWAccelConfig` (a pure accessor) and the constructors. This covers `Start`, `Segment`, `Metadata`, `ProbeMetadata`, `MasterPlaylist`, `VariantPlaylist`, `SubtitlePlaylist`, `SpriteVTT`, `Sprite`, `SubtitleVTT`, `SubtitlesVTT`, `Preview`, `Thumbnail`, `MediaKey` and `Invalidate`.

With `HWAccel: true` or an explicit `HWAccelMode`, construction runs a hardware self-test. An explicit `HWAccelMode` is validated once with a 15-second bound. Auto-detection (`HWAccel: true`) spends up to 10 seconds detecting accelerators, then validates each detected one in the order CUDA, QSV, VideoToolbox, VAAPI with a 15-second bound per accelerator and keeps the first that passes. On a host where all four are present and every one fails slowly, `NewController` can therefore take up to 70 seconds before it falls back to software. Pass a context with a deadline to `NewControllerWithContext` to cap it.

### Playlists and segments

| Method | Description |
|---|---|
| `MasterPlaylist(ctx, sourceURL) (string, error)` | Master playlist with video, audio and subtitle renditions. |
| `VariantPlaylist(ctx, sourceURL, streamType, rendition) (string, error)` | Media playlist for one video or audio rendition. |
| `SubtitlePlaylist(ctx, sourceURL, trackID) (string, error)` | Media playlist for one subtitle track. |
| `Segment(ctx, sourceURL, streamType, rendition, index) ([]byte, error)` | Segment data; transcoded on first request, cached after. `streamType` is `StreamVideo` or `StreamAudio`. |

### Metadata and identity

| Method | Description |
|---|---|
| `Metadata(ctx, sourceURL) (*Metadata, error)` | Probe result, cached in `Storage`. |
| `ProbeMetadata(ctx, sourceURL) (*Metadata, error)` | Probes without reading or writing the cache. Bounded by `ProbeTimeout`. |
| `MediaKey(ctx, sourceURL) (string, error)` | The resolved opaque media key. |
| `Invalidate(ctx, sourceURL) error` | Deletes everything stored for the media, including `SpriteStorage` when configured. Returns `ErrCacheInvalidationUnsupported` if `Storage` (or a configured `SpriteStorage`) has no `DeleteMedia`. |

`Metadata` carries `StartTime`, `Duration`, `Bitrate` (container bits per second, 0 if unknown), `Keyframes`, one `Video` stream, and `Audios` and `Subtitles` lists. `VideoStream.End` and `AudioStream.End` are the stream ends in seconds on the `Metadata` timeline, 0 if unknown. When the container declares no end they are measured from the last packets (a read of the last 15 seconds at probe time, then a bounded search of the rest of the file for a stream that ended earlier; they stay 0 if the reads take over 20 seconds). MPEG-TS stream ends are always measured, and a Matroska stream end comes from its `DURATION` tag or is measured, never from ffprobe's stream duration. An audio end that lies past the readable data (a truncated audio-only source, or audio cut after the video ended) is lowered to the last readable packet, and `Duration` with it. `Duration` is bounded by the known stream ends; when the container declares none, it is the latest stream end, audio included. `VideoStream.FrameRate` is the average frame rate when known, so a 50 fields-per-second interlaced source reports 25. Each subtitle has a stable `ID` (for example `subtitle-3`) used as the track ID everywhere else. `Metadata` lists every subtitle track, including ones that cannot be served (see below).

Probing reads an MP4 from its `moov` box and an MKV or WebM from its Matroska Cues element, so neither needs a full packet scan or download. Cues are used when they leave no keyframe gap above 11.5 seconds; otherwise the probe falls back to the scan. A real I/O error reading the MP4 header (local or remote) fails the probe rather than falling back to the packet scan. Remote MP4s are indexed from the `moov` box, which reads a small fraction of the file.

A probe that fails is remembered in process for 10 seconds (at most 1024 sources are remembered; the earliest to expire is evicted first), so repeated requests for a broken source return the same error without re-running ffprobe. `Invalidate(sourceURL)` forgets that source's entry, and a probe that began before any `Invalidate` call is not remembered.

### Seek previews and subtitle files

| Method | Description |
|---|---|
| `SpriteVTT(ctx, sourceURL) ([]byte, error)` | Thumbnail-sprite WebVTT, rendered on every request with the current `PathGenerator.Sprite`. Generates the sprite sheets if they are missing. |
| `Sprite(ctx, sourceURL, index) ([]byte, error)` | One sprite sheet image. |
| `SubtitleVTT(ctx, sourceURL, trackID) ([]byte, error)` | WebVTT for one subtitle track. |
| `SubtitlesVTT(ctx, sourceURL, trackIDs) (map[string]SubtitleResult, error)` | Several tracks in one pass. `SubtitleResult` has `Data []byte` and `Err error` per track. |
| `Preview(ctx, sourceURL, *PreviewConfig) ([]byte, error)` | Animated preview. `nil` uses `DefaultPreviewConfig`; zero fields fall back to the defaults. |
| `Thumbnail(ctx, sourceURL, *ThumbnailConfig) ([]byte, error)` | Single image. `nil` uses `DefaultThumbnailConfig`; a zero `Position` or `Width` means "use the default". `Position` is a fraction of the duration in `(0, 1]`, so the very first frame is not selectable; use a small positive value such as `0.001`. |

```go
type PreviewConfig struct {
    MaxFrames     int // 1..1000
    Frameduration int // milliseconds per frame, 1..60000
    Width         int // pixels, 1..4096
}

type ThumbnailConfig struct {
    Position float64 // (0, 1]; 0 means the default
    Width    int     // pixels, 1..8192
}
```

Only text subtitle codecs are served: the allowlist is in `internal/subtitle/codec.go` and covers SubRip, ASS/SSA, `mov_text`/`tx3g`, WebVTT/`wvtt`, MicroDVD, SAMI and similar text formats, plus `eia_608`, which converts only from standalone caption inputs such as `.scc`. Bitmap tracks (PGS, DVD, DVB, XSUB, teletext) and unlisted codecs such as TTML are not listed in the master playlist, and `SubtitlePlaylist`, `SubtitleVTT` and `SubtitlesVTT` return an error wrapping `ErrRenditionNotFound` for them (for `SubtitlesVTT`, in that track's `SubtitleResult.Err`). Unknown track IDs return the same error, with text of the form `rendition not found: subtitle track X`.

For progressive MP4 sources, Goshl reads the MP4 index and fetches only the bytes belonging to requested `mov_text`/`tx3g` or `wvtt` subtitle samples. Local files use random access and HTTP sources use validated byte-range requests. For MKV sources whose Cues cover the requested subtitle tracks, only the cued blocks are read and ffmpeg converts them, with output identical to a full read; other tracks use one full pass shared by all tracks requested together. The generated WebVTT is stored through `Storage`, so later requests do not reopen the source.

Unsupported containers, fragmented MP4 layouts, and HTTP servers without byte-range support continue through the FFmpeg fallback. A failed single-track batch is not rerun with the identical command, and a partial batch output is never stored. Subtitle extraction remains request driven; probing and ingest do not extract subtitle payloads.

## Options

Fields left at their zero value take the default shown.

```go
goshl.Options{
    Storage:     myStorage,     // required
    Coordinator: myCoordinator, // required
    PathGen:     myPathGen,     // required

    SpriteStorage: nil, // optional separate store for sprite sheets

    HWAccel:               false,           // auto-detect GPU encoding
    HWAccelMode:           "",              // AccelNone, AccelCUDA, AccelVideoToolbox, AccelVAAPI or AccelQSV; overrides HWAccel
    HWAccelDecodeMode:     goshl.DecodeSafe, // DecodeSafe or DecodeDevice
    AllowHardwareFallback: false,           // fall back to software when an explicit HWAccelMode fails validation

    SegmentTimeout:    30 * time.Second, // max wait for one Segment call
    JobTimeout:        2 * time.Minute,  // execution budget of one FFmpeg job
    ProbeTimeout:      0,                // defaults to JobTimeout
    GenerationTimeout: 10 * time.Minute, // sprites, previews, thumbnails, subtitles

    TargetDuration: 6.0, // target segment duration in seconds
    SegmentsPerJob: 4,   // segments per transcoding job
    VideoPoolSize:  2,   // video workers
    AudioPoolSize:  4,   // audio workers

    DefaultPreviewConfig:   goshl.PreviewConfig{MaxFrames: 40, Frameduration: 250, Width: 480},
    DefaultThumbnailConfig: goshl.ThumbnailConfig{Position: 0.1, Width: 480},

    OmitSubtitleRenditions: false, // leave subtitle groups out of the master playlist

    CacheVersion:   "",     // change when host-side media policy changes
    SourceRevision: nil,    // func(ctx, sourceURL) (string, error)
    Observer:       nil,    // receives lifecycle events
}
```

Negative durations, sizes, or out-of-range preview and thumbnail values make construction return `ErrInvalidOptions` (or `ErrInvalidPreviewConfig` / `ErrInvalidThumbnailConfig` wrapped in it). `TargetDuration` must be 1-60 seconds, `SegmentsPerJob` at most 1000 and pool sizes at most 1024; violations wrap `ErrInvalidOptions`. Segments are at most `max(2 x TargetDuration, 12 s)` long; see Playback behavior.

A segment request waits up to `SegmentTimeout` for the segment, but the job itself is never cancelled because a waiter left. A queued job that does not start before its `StartBy` (enqueue time plus `JobTimeout`) is settled as failed with a start-by error rather than run.

Local files use size and modification time as their default source revision. Remote URLs are treated as immutable unless `SourceRevision` returns an ETag, version, or content fingerprint. Call `controller.MediaKey` to inspect the resolved opaque key.

## Hardware acceleration

Set `HWAccel: true` to auto-detect GPU encoding. Supported accelerators are NVIDIA CUDA/NVENC, Intel QSV, Apple VideoToolbox and VAAPI. Detection runs a bounded decode/filter/encode self-test on each detected accelerator in the order CUDA, QSV, VideoToolbox, VAAPI and selects the first that passes. If none passes, auto mode falls back to software. `controller.HWAccelConfig().Error` and the `EventHardwareFallback` event list the failure of every accelerator that was skipped, including when a lower-priority accelerator was selected. Software encoding uses libx264 `superfast`, the fastest preset that produces the High profile the playlists advertise. VideoToolbox encodes with `-g 250`, so only the forced segment keyframes appear.

Set `HWAccelMode` when the caller already knows the desired accelerator:

```go
controller, err := goshl.NewControllerWithContext(ctx, goshl.Options{
    Storage:           myStorage,
    Coordinator:       myCoordinator,
    PathGen:           myPathGen,
    HWAccelMode:       goshl.AccelCUDA,
    HWAccelDecodeMode: goshl.DecodeSafe,
})
```

`DecodeSafe` is the CUDA default: NVDEC is used without retaining decoder frames in the CUDA surface pool, then scaling runs on host frames before NVENC. This is the compatible path for the Blackwell decoder failure identified in the lab. `DecodeDevice` opts into the full device-frame decode/scale/encode graph. Explicit hardware selection fails construction when its self-test fails unless `AllowHardwareFallback` is enabled.

```go
cfg := controller.HWAccelConfig()
log.Printf("accelerator=%s requested=%s available=%v error=%s", cfg.Accelerator, cfg.Requested, cfg.Available, cfg.Error)
```

Workers on different accelerators use different pipeline profiles and never exchange jobs, so mixed-hardware fleets keep separate queues.

## Other exported types

- `PlaybackMethod`: `DirectStream` or `Transcode`, set on each `VideoRendition` and `AudioRendition` in a `Job`.
- `ColourPlan`: the type of `VideoRendition.Colour` (`Tonemap`, `AssumeMatrix`, `AssumePrimaries`).
- `StreamType`: `StreamVideo`, `StreamAudio` or `StreamSubtitle` (subtitles are served through `SubtitlePlaylist` and `SubtitleVTT`; `Segment` rejects them).
- `SegmentState` / `SegmentStatus`: `SegmentStateReady` or `SegmentStateError`, with an `Error` string, passed to `Coordinator.NotifySegment`.
- `SourceRevisionResolver`: `func(ctx context.Context, sourceURL string) (string, error)`, the type of `Options.SourceRevision`.
- `CacheInvalidator`: `DeleteMedia(ctx context.Context, mediaKey string) error`, implemented by `Storage` and `SpriteStorage` to support `Invalidate`.
- `HWAccelConfig` also carries `Encoder`, `KeyframeFlag`, `ScaleFilter`, `DecodeMode` and `DeviceFrames`, describing the FFmpeg setup selected.

## Observability

`Observer` receives an `Event` for each of the following. Event identities are opaque hashes; raw signed source URLs are not emitted. Callbacks should return promptly. `ObserverFunc` adapts a plain function.

| Event type | Meaning |
|---|---|
| `EventMetadataCacheHit`, `EventMetadataCacheMiss`, `EventMetadataProbed` | Metadata cache lookups and fresh probes. A miss caused by unreadable stored metadata carries `Error` ("corrupt cached metadata: ...") and the metadata is probed again. |
| `EventSegmentCacheHit`, `EventSegmentCacheMiss` | Segment cache lookups. |
| `EventLockWait` | A request is waiting on a lock another replica holds: a segment batch, a metadata probe, or a generated asset (sprites, previews, thumbnails, subtitles). |
| `EventLockReleaseFailed` | Releasing a batch lock failed. |
| `EventJobEnqueued`, `EventJobJoined` | This replica enqueued a job, or joined one already running. |
| `EventJobStarted`, `EventJobCompleted`, `EventJobFailed` | Worker-side job lifecycle. |
| `EventJobAckFailed`, `EventJobLeaseFailed`, `EventJobSettleFailed` | Coordinator calls failed during delivery handling. |
| `EventJobDeliveryRejected` | A worker refused a malformed delivery (wrong profile, invalid job, or a job whose `Digest` does not match its fields). A job whose `StartBy` already passed is not a rejection: it is settled failed and reported as `EventJobFailed`. |
| `EventPoolSubscriptionLost` | A worker pool lost its coordinator subscription. |
| `EventSegmentReady`, `EventSegmentNotifyFailed` | `EventSegmentReady`: a waiting request learned of its segment through the coordinator's readiness notification (requests satisfied directly from storage do not emit it). `EventSegmentNotifyFailed`: the worker could not publish the notification. |
| `EventHardwareSelected`, `EventHardwareFallback` | Hardware choice at construction, and the reasons when a preferred accelerator was skipped (for a lower-priority one or for software). |
| `EventRenditionDropped` | A video rendition was left out of the ladder (it would need an H.264 level above 5.2). Emitted per dropped rendition on each master playlist request; `Rendition` and `Error` carry the name and the reason. |

FFmpeg and FFprobe stderr are bounded, and source URLs, credentials, query strings, and media paths are redacted from returned command errors.

## Errors

| Error | Returned when |
|---|---|
| `ErrInvalidOptions` | `Options` failed validation, or `ffmpeg` or `ffprobe` is older than 6.1. |
| `ErrUnsupportedSource` | The source is empty, uses a scheme other than file/http/https, or is a malformed URL. |
| `ErrControllerStopped` | The controller was stopped; build a new one. Also returned by `Start` when `Stop` has already run, and to waiters on a lock another replica holds when `Stop` runs. |
| `ErrInvalidPreviewConfig`, `ErrInvalidThumbnailConfig` | A preview or thumbnail config is out of range. |
| `ErrInvalidStreamType` | `Segment` or `VariantPlaylist` got a stream type other than video or audio. |
| `ErrRenditionNotFound` | The rendition name is invalid or not offered for this source, or the subtitle track ID is unknown or its codec cannot be converted to WebVTT. |
| `ErrSegmentOutOfRange` | Negative or past-the-end segment index, or a source with no playable segments. |
| `ErrSegmentTimeout` | A segment did not become available within `SegmentTimeout`. |
| `ErrMetadataTimeout` | Metadata did not become available in time, including when a probe (this replica's, or the replica holding the lock) hit `ProbeTimeout`; the error also wraps `context.DeadlineExceeded` and the probe error. |
| `ErrCacheInvalidationUnsupported` | `Invalidate` was called but storage has no `DeleteMedia`. |
| `ErrLockHeld` | Returned by `Coordinator.AcquireJob` / `AcquireLock` when the lock is taken. |
| `ErrEnqueueRejected` | Wrap in `Coordinator.Enqueue` errors when the job was definitely not persisted. |

Match with `errors.Is`.

## Playback behavior

Segments last at most `max(2 x TargetDuration, 12 s)`. A keyframe gap longer than that is split into equal pieces, the planner cuts early at the previous keyframe to stay under the cap, and a tail sliver shorter than `min(0.5 s, TargetDuration / 2)` merges into the previous segment. Rounding noise in keyframe times (for example millisecond-rounded MKV cues) does not merge segments.

Direct H.264 remuxing (the `DirectStream` method) is restricted to streams that are all of the following: H.264 Baseline, Main or High at a known level of 5.2 or lower; 8-bit `yuv420p` or `yuvj420p`; starting with a keyframe (within 1.5 ms of the video start); no rotation and no anamorphic sample aspect ratio; not tagged PQ or HLG; not ending more than 0.5 s before the end of the file; and with every keyframe gap within the segment cap (12 s at the default target; a source with no keyframe list is never copied). Other inputs transcode to High-profile `yuv420p` H.264, which includes the source-resolution rung of a source that cannot be copied.

Every transcode is tagged BT.709 limited range. Untagged sources are assumed BT.709 (1280 pixels wide or more, or over 576 lines) or SMPTE 170M, and full-range sources are converted rather than emitted as `yuvj420p`. HDR (PQ or HLG) sources are tone mapped to SDR in software with `zscale` and `tonemap`; see the runtime requirements.

Transcoded audio from sources at 44.1 kHz or 32 kHz joins frame-exactly across jobs. Sources whose video starts after the container start are cut on the plan, and every job's output is validated against its expected segment times; a mismatch fails the job instead of caching misplaced segments. When the video ends before the audio, the remaining video segments repeat the last frame.

Master playlists distinguish audio codec and channel groups, declare aggregate bandwidth and codecs, support silent and audio-only media, and expose subtitle groups (unless `OmitSubtitleRenditions` is set). Each audio group has the same members, one per source track, with `NAME` taken from the track title or language label, exactly one `DEFAULT` per group, and `LANGUAGE` as an RFC 5646 tag (`en`, not `eng`). Subtitle `DEFAULT` follows the source disposition, subtitle names are unique, and `FORCED=YES` implies `AUTOSELECT=YES`. An audio-only master carries `BANDWIDTH` and `SUBTITLES`. `MasterPlaylist`, `SubtitlePlaylist` and `VariantPlaylist` return an error rather than write an invalid playlist (no renditions, zero bitrate, empty codec, zero duration or no segments).

## ffmpeg base image

`docker/ffmpeg` builds a runtime base image with ffmpeg and ffprobe for goshl hosts. Tags are immutable and never move; pin the digest.

| Tag | ffmpeg | Platforms | Digest |
|---|---|---|---|
| `ghcr.io/eleven-am/goshl-ffmpeg:9.0.2-1` | 9.0.2 | linux/amd64, linux/arm64 | `sha256:49618bfc33fd0a8e9705be5aae97129172b596bc3a1b69a9fd03664a2ccfcaef` |

- Built from the official ffmpeg source, as a shared-library build on `ubuntu:24.04`, against nv-codec-headers 13.0.19.0 (NVENC API 13.0). NVENC therefore runs on NVIDIA driver 570 or newer.
- Includes `libx264`, `h264_nvenc`, `h264_vaapi`, `h264_qsv` (amd64), `aac`, `ac3`, `eac3`, `png`, `mjpeg` and the subtitle codecs. Filters include `scale_cuda`, `zscale`, `tonemap`, `tonemap_vaapi` and `tonemap_opencl`.
- The runtime has `gosu`, `passwd`, `ca-certificates`, `tzdata` and `wget`, with `NVIDIA_DRIVER_CAPABILITIES=compute,utility,video`.
- `docker/ffmpeg/selfcheck.sh` runs as the last build step. The build fails unless every required encoder, filter, muxer and library is present, NVENC targets API 13.0, and software, HLS, HDR tone-mapping and subtitle smoke encodes succeed. Hardware encoders are checked for presence only.
- To release a new image, push a tag `ffmpeg-<version>-<n>` to run `.github/workflows/ffmpeg-image.yml`. It builds each platform on native runners and refuses to overwrite an existing tag. To build locally, run `docker buildx build --platform linux/amd64,linux/arm64 -t <image>:<tag> --push docker/ffmpeg`.

## Upgrading

See [MIGRATION.md](MIGRATION.md) for cache, job-contract and rollout changes between versions, and [CHANGELOG.md](CHANGELOG.md) for history.

## Author

Roy Ossai

## License

GPL-3.0
