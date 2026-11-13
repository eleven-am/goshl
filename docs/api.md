# API reference

Package `github.com/eleven-am/goshl`. Match errors with `errors.Is`. Every `Controller` method except `Stop`, `HWAccelConfig` and the constructors returns `ErrControllerStopped` after `Stop`, before it validates arguments.

```go
controller, err := goshl.NewControllerWithContext(ctx, goshl.Options{Storage: s, Coordinator: c, PathGen: p})
if err != nil {
	return err
}
if err := controller.Start(ctx); err != nil {
	return err
}
defer controller.Stop()
playlist, err := controller.MasterPlaylist(ctx, "/media/film.mkv")
```

`sourceURL` is a local path, a `file:` URL or an `http(s)` URL. Rendition names contain letters, digits, `.`, `_` and `-`, at most 64 characters, never start with `.` and never contain `..`.

## Controller

### Construction and lifecycle

| Symbol | Purpose |
|---|---|
| `NewControllerWithContext(ctx, Options) (*Controller, error)` | Validates options, checks ffmpeg versions, selects hardware. Errors wrap `ErrInvalidOptions`. |
| `NewController(Options) *Controller` | Same, panics on error. |
| `(*Controller).Start(ctx) error` | Starts the video and audio worker pools. Returns `ErrControllerStopped` if `Stop` already ran. |
| `(*Controller).Stop()` | Idempotent. Cancels detached work, waits up to 10 seconds, releases locks of flights never enqueued. A stopped controller cannot restart. Locks of enqueued jobs expire by TTL. |
| `(*Controller).HWAccelConfig() HWAccelConfig` | Copy of the selected hardware configuration. |

```go
cfg := controller.HWAccelConfig()
log.Printf("accelerator=%s requested=%s error=%s", cfg.Accelerator, cfg.Requested, cfg.Error)
```

### Playlists and segments

| Method | Purpose |
|---|---|
| `MasterPlaylist(ctx, sourceURL) (string, error)` | Master playlist with video, audio and subtitle renditions. |
| `VariantPlaylist(ctx, sourceURL, streamType, renditionName) (string, error)` | Media playlist of one video or audio rendition. |
| `SubtitlePlaylist(ctx, sourceURL, trackID) (string, error)` | Media playlist of one subtitle track. |
| `Segment(ctx, sourceURL, streamType, renditionName, index) ([]byte, error)` | One segment. Transcoded on first request, cached after. |

`Segment` and `VariantPlaylist` accept `StreamVideo` and `StreamAudio`. Other values return `ErrInvalidStreamType`.

```go
data, err := controller.Segment(ctx, src, goshl.StreamVideo, "720p", 10)
```

Playlist methods return an error instead of an invalid playlist. `MasterPlaylist` and the video `VariantPlaylist` of an HDR source return an error wrapping `ErrTonemapUnavailable` when ffmpeg lacks `zscale` or `tonemap`.

### Metadata and identity

| Method | Purpose |
|---|---|
| `Metadata(ctx, sourceURL) (*Metadata, error)` | Probe result, cached in `Storage`. |
| `ProbeMetadata(ctx, sourceURL) (*Metadata, error)` | Probe without reading or writing the cache. Bounded by `ProbeTimeout`. |
| `MediaKey(ctx, sourceURL) (string, error)` | The opaque media key for the source. |
| `Invalidate(ctx, sourceURL) error` | Deletes everything stored for the media, in `Storage` and `SpriteStorage`. Returns `ErrCacheInvalidationUnsupported` when a store lacks `DeleteMedia`. |

A failed probe is remembered in process for 10 seconds, up to 1024 sources. `Invalidate` forgets it.

### Seek previews, subtitles and images

| Method | Purpose |
|---|---|
| `SpriteVTT(ctx, sourceURL) ([]byte, error)` | Sprite WebVTT, rendered on every request with `PathGenerator.Sprite`. Generates missing sprite sheets. |
| `Sprite(ctx, sourceURL, index) ([]byte, error)` | One sprite sheet image. |
| `SubtitleVTT(ctx, sourceURL, trackID) ([]byte, error)` | WebVTT of one subtitle track. |
| `SubtitlesVTT(ctx, sourceURL, trackIDs) (map[string]SubtitleResult, error)` | Several tracks in one pass. |
| `Preview(ctx, sourceURL, *PreviewConfig) ([]byte, error)` | Animated preview. `nil` uses `DefaultPreviewConfig`. |
| `Thumbnail(ctx, sourceURL, *ThumbnailConfig) ([]byte, error)` | Single image. `nil` uses `DefaultThumbnailConfig`. |

```go
results, err := controller.SubtitlesVTT(ctx, src, []string{"subtitle-2", "subtitle-3"})
for id, r := range results {
	if r.Err != nil {
		log.Printf("%s: %v", id, r.Err)
	}
}
```

Only text subtitle codecs are served: SubRip, ASS/SSA, `mov_text`/`tx3g`, WebVTT/`wvtt`, MicroDVD, SAMI and similar, plus `eia_608` from standalone caption inputs. Bitmap tracks and unlisted codecs return an error wrapping `ErrRenditionNotFound`, as does an unknown track ID.

## Types

### Options

See [configuration.md](configuration.md). `Options` has the fields `Storage`, `Coordinator`, `PathGen`, `HWAccel`, `HWAccelMode`, `SegmentTimeout`, `TargetDuration`, `SegmentsPerJob`, `VideoPoolSize`, `AudioPoolSize`, `DefaultPreviewConfig`, `DefaultThumbnailConfig`, `JobTimeout`, `ProbeTimeout`, `GenerationTimeout`, `SpriteStorage`, `CacheVersion`, `SourceRevision`, `Observer`, `HWAccelDecodeMode`, `AllowHardwareFallback` and `OmitSubtitleRenditions`.

### Interfaces you implement

#### Storage

Persists generated data. Keys are opaque: store them exactly as given and never derive them from the source URL.

| Method | Purpose |
|---|---|
| `MetadataExists(ctx, mediaKey) (bool, error)` | Reports stored metadata. |
| `GetMetadata(ctx, mediaKey) ([]byte, error)` | Reads metadata. |
| `SetMetadata(ctx, mediaKey, data) error` | Writes metadata. |
| `WriteSegment(ctx, SegmentData, data) error` | Writes a segment. |
| `ReadSegment(ctx, SegmentData) ([]byte, error)` | Reads a segment. |
| `SegmentExists(ctx, SegmentData) (bool, error)` | Reports a stored segment. |
| `WriteSprite(ctx, assetKey, index, data) error` | Writes a sprite sheet. |
| `ReadSprite(ctx, assetKey, index) ([]byte, error)` | Reads a sprite sheet. |
| `SpriteExists(ctx, assetKey, index) (bool, error)` | Reports a stored sprite sheet. |
| `WriteSubtitleVTT(ctx, assetKey, trackID, data) error` | Writes a subtitle track. |
| `ReadSubtitleVTT(ctx, assetKey, trackID) ([]byte, error)` | Reads a subtitle track. |
| `SubtitleVTTExists(ctx, assetKey, trackID) (bool, error)` | Reports a stored subtitle track. |
| `WritePreview(ctx, assetKey, data) error` | Writes a preview. |
| `ReadPreview(ctx, assetKey) ([]byte, error)` | Reads a preview. |
| `PreviewExists(ctx, assetKey) (bool, error)` | Reports a stored preview. |
| `WriteThumbnail(ctx, assetKey, data) error` | Writes a thumbnail. |
| `ReadThumbnail(ctx, assetKey) ([]byte, error)` | Reads a thumbnail. |
| `ThumbnailExists(ctx, assetKey) (bool, error)` | Reports a stored thumbnail. |

```go
func (s *store) WriteSegment(ctx context.Context, info goshl.SegmentData, data []byte) error {
	key := fmt.Sprintf("%s/%s/%t/%d.ts", info.MediaKey, info.Rendition, info.IsVideo, info.Index)
	return s.put(ctx, key, data)
}
```

#### SpriteStorage

Optional sprite-only store set through `Options.SpriteStorage`. It has `WriteSprite`, `ReadSprite` and `SpriteExists` with the same signatures as `Storage`.

#### CacheInvalidator

```go
type CacheInvalidator interface {
	DeleteMedia(context.Context, string) error
}
```

Implement it on `Storage` and on `SpriteStorage` to enable `Controller.Invalidate`. `DeleteMedia` removes everything stored under a media key.

#### Coordinator

Distributes jobs and holds locks. The full contract is in [operations.md](operations.md#coordinator-contract).

| Method | Purpose |
|---|---|
| `Enqueue(ctx, Job) error` | Queues a job. Idempotent by `Job.RunID`. |
| `Subscribe(ctx, StreamType, profile) (<-chan JobDelivery, error)` | Competing-consumer subscription by stream type and profile. |
| `Renew(ctx, receipt, ttl) error` | Extends one delivery lease. |
| `Ack(ctx, receipt) error` | Discards a malformed delivery. |
| `SettleJob(ctx, receipt, JobStatus) error` | Persists a terminal status and removes the delivery. |
| `WaitJob(ctx, runID) (<-chan JobStatus, error)` | Streams the status of one run. |
| `NotifySegment(ctx, SegmentData, SegmentStatus) error` | Publishes segment readiness. |
| `WaitSegment(ctx, SegmentData) (<-chan SegmentStatus, error)` | Waits for segment readiness. |
| `AcquireJob(ctx, jobID, runID, ttl) (token, activeRunID string, err error)` | Takes the batch lock and registers the run. |
| `ReleaseJob(ctx, jobID, runID, token) error` | Releases the batch lock. |
| `AcquireLock(ctx, key, ttl) (token string, err error)` | Takes a named lock. |
| `ReleaseLock(ctx, key, token) error` | Releases a named lock. |

#### PathGenerator

Builds the URLs embedded in playlists. Each URL routes to one of your handlers.

| Method | Purpose |
|---|---|
| `VariantPlaylist(sourceURL, rendition string, streamType StreamType) string` | URL of a variant playlist. |
| `Segment(sourceURL, rendition string, streamType StreamType, index int) string` | URL of a segment. |
| `Sprite(sourceURL string, index int) string` | URL of a sprite sheet. |
| `SubtitlePlaylist(sourceURL, trackID string) string` | URL of a subtitle playlist. |
| `SubtitleVTT(sourceURL, trackID string) string` | URL of a subtitle WebVTT file. |

#### Observer

```go
type Observer interface {
	Observe(context.Context, Event)
}
```

`ObserverFunc` adapts a function to `Observer`.

```go
Options{Observer: goshl.ObserverFunc(func(ctx context.Context, e goshl.Event) {
	log.Printf("%s %s", e.Type, e.Error)
})}
```

#### SourceRevisionResolver

`func(context.Context, string) (string, error)`. The type of `Options.SourceRevision`. It returns the revision of a source.

### Metadata types

| Type | Fields |
|---|---|
| `Metadata` | `StartTime`, `Duration`, `Bitrate` (container bits per second, 0 if unknown), `Keyframes`, `KeyframeBytes`, `Video` (`VideoStream`), `Audios` (`[]AudioStream`), `Subtitles` (`[]SubtitleStream`) |
| `VideoStream` | `Index`, `Codec`, `CodecTag`, `Profile`, `Level`, `PixelFormat`, `BitDepth`, `Width`, `Height`, `Bitrate`, `FrameRate`, `ColorRange`, `ColorSpace`, `ColorTransfer`, `ColorPrimaries`, `SampleAspect`, `Rotation`, `Default`, `ClosedCaptions`, `StartsWithKeyframe`, `Start`, `End` |
| `AudioStream` | `Index`, `Codec`, `Language`, `Title`, `Channels`, `SampleRate`, `ChannelLayout`, `Bitrate`, `Default`, `End` |
| `SubtitleStream` | `Index`, `ID`, `Codec`, `Language`, `Title`, `Forced`, `Default` |

`End` is the stream end in seconds on the `Metadata` timeline, 0 if unknown. `VideoStream.FrameRate` is the average frame rate. `SubtitleStream.ID` (for example `subtitle-3`) is the track ID used by every subtitle method. `Metadata.Subtitles` lists every track, including ones that cannot be served.

### Request and result types

| Type | Definition |
|---|---|
| `PreviewConfig` | `MaxFrames int` (1 to 1000), `Frameduration int` (ms, 1 to 60000), `Width int` (1 to 4096). |
| `ThumbnailConfig` | `Position float64` (0 to 1, 0 is the default), `Width int` (1 to 8192). |
| `SubtitleResult` | `Data []byte`, `Err error` for one track of `SubtitlesVTT`. |
| `StreamType` | `StreamVideo`, `StreamAudio`, `StreamSubtitle`. |
| `Segment` | `Index int`, `Start`, `End`, `Duration float64` in seconds. |
| `SegmentData` | `MediaKey string`, `Index int`, `Rendition string`, `IsVideo bool`. |
| `SegmentState` | `SegmentStateReady`, `SegmentStateError`. |
| `SegmentStatus` | `State SegmentState`, `Error string`. Passed to `NotifySegment`. |

### Hardware types

| Type | Definition |
|---|---|
| `Accelerator` | `AccelNone`, `AccelCUDA`, `AccelVideoToolbox`, `AccelVAAPI`, `AccelQSV`. |
| `DecodeMode` | `DecodeSafe`, `DecodeDevice`. |
| `HWAccelConfig` | `Accelerator` (selected), `Requested`, `Available []Accelerator`, `Error` (skipped accelerators and their failures), `DecodeFlags`, `EncodeFlags`, `Encoder`, `KeyframeFlag`, `ScaleFilter`, `DecodeMode`, `DeviceFrames`. |

### Job types

| Type | Definition |
|---|---|
| `Job` | One unit of work. See [operations.md](operations.md#coordinator-contract). |
| `JobDelivery` | `Job Job`, `Receipt string`. |
| `JobState` | `JobStateCompleted`, `JobStateFailed`, `JobStateCancelled`. All are terminal. |
| `JobStatus` | `JobID`, `RunID string`, `State JobState`, `Error string`. |
| `VideoRendition` | `Name`, `Width`, `Height`, `Bitrate`, `PeakBitrate`, `BufferSize`, `PeakBandwidth`, `AverageBandwidth`, `FrameRate`, `Codec`, `Level`, `Method`, `Colour`, `StripParameterSets`, `StripSEI`, `SourceEnd`. |
| `AudioRendition` | `Name`, `Codec`, `CodecString`, `Bitrate`, `Channels`, `SampleRate`, `PeakBandwidth`, `AverageBandwidth`, `SourceEnd`, `Method`, `StreamIndex`, `Language`, `Title`, `Default`. |
| `PlaybackMethod` | `DirectStream` (remux, copy) or `Transcode`. |
| `ColourPlan` | `Tonemap`, `Retag`, `AssumeMatrix`, `AssumePrimaries`. Tells the worker how to tag, convert or tone map. |

## Events

`Event` fields: `Type`, `Time`, `MediaKey`, `JobID`, `StreamType`, `Rendition`, `Index`, `StartIndex`, `EndIndex`, `Duration`, `Bytes`, `Accelerator`, `Error`. `EventType` is a string. Identities are opaque hashes.

| Constant | Value | Emitted when |
|---|---|---|
| `EventMetadataCacheHit` | `metadata.cache.hit` | Stored metadata was found. |
| `EventMetadataCacheMiss` | `metadata.cache.miss` | No usable stored metadata. `Error` is set for unreadable metadata. |
| `EventMetadataProbed` | `metadata.probed` | A fresh probe finished. |
| `EventSegmentCacheHit` | `segment.cache.hit` | A segment came from storage. |
| `EventSegmentCacheMiss` | `segment.cache.miss` | A segment was not stored. |
| `EventLockWait` | `segment.lock.wait` | A request waits on a lock another replica holds. |
| `EventJobEnqueued` | `job.enqueued` | This replica enqueued a job. |
| `EventJobJoined` | `job.joined` | This replica joined a job already running. |
| `EventJobStarted` | `job.started` | A worker started a job. |
| `EventJobCompleted` | `job.completed` | A worker finished a job. |
| `EventJobFailed` | `job.failed` | A job failed, or its `StartBy` passed. |
| `EventJobAckFailed` | `job.ack.failed` | `Ack` failed. |
| `EventJobLeaseFailed` | `job.lease.failed` | `Renew` failed. |
| `EventJobSettleFailed` | `job.settle.failed` | `SettleJob` failed. |
| `EventJobDeliveryRejected` | `job.delivery.rejected` | A worker refused a malformed delivery. |
| `EventPoolSubscriptionLost` | `pool.subscription.lost` | A worker pool lost its subscription. |
| `EventSegmentReady` | `segment.ready` | A waiting request learned of its segment through `WaitSegment`. |
| `EventSegmentNotifyFailed` | `segment.notify.failed` | A worker could not publish readiness. |
| `EventHardwareSelected` | `hardware.selected` | Hardware was chosen at construction. |
| `EventHardwareFallback` | `hardware.fallback` | A preferred accelerator was skipped. |
| `EventLockReleaseFailed` | `lock.release.failed` | Releasing a batch lock failed. |
| `EventRenditionDropped` | `rendition.dropped` | A video rung was left out. Emitted per rung on each `MasterPlaylist` call. |

## Errors

| Error | Returned when |
|---|---|
| `ErrInvalidOptions` | `Options` is invalid, or ffmpeg or ffprobe is older than 6.1. |
| `ErrInvalidStreamType` | `Segment` or `VariantPlaylist` got a stream type other than video or audio. |
| `ErrRenditionNotFound` | The rendition name is invalid or not offered, or the subtitle track is unknown or unconvertible. |
| `ErrSegmentOutOfRange` | The index is negative, past the end, or the source has no playable segments. |
| `ErrSegmentTimeout` | A segment did not appear within `SegmentTimeout`. |
| `ErrMetadataTimeout` | Metadata did not appear in time, or a probe hit `ProbeTimeout`. Also wraps `context.DeadlineExceeded`. |
| `ErrCacheInvalidationUnsupported` | `Invalidate` found a store without `DeleteMedia`. |
| `ErrInvalidPreviewConfig` | A preview config is out of range. |
| `ErrInvalidThumbnailConfig` | A thumbnail config is out of range. |
| `ErrControllerStopped` | The controller is stopped. Build a new one. |
| `ErrEnqueueRejected` | Wrap it in a `Coordinator.Enqueue` error when the job was definitely not persisted. |
| `ErrLockHeld` | Return it from `AcquireJob` or `AcquireLock` when the lock is taken. |
| `ErrTonemapUnavailable` | An HDR source needs tone mapping and ffmpeg lacks `zscale` or `tonemap`. |
| `ErrUnsupportedSource` | The source is empty, malformed, or uses a scheme other than file, http or https. |

## Constants

| Group | Constants |
|---|---|
| Stream types | `StreamVideo`, `StreamAudio`, `StreamSubtitle` |
| Accelerators | `AccelNone`, `AccelCUDA`, `AccelVideoToolbox`, `AccelVAAPI`, `AccelQSV` |
| Segment states | `SegmentStateReady`, `SegmentStateError` |
| Job states | `JobStateCompleted`, `JobStateFailed`, `JobStateCancelled` |
| Decode modes | `DecodeSafe`, `DecodeDevice` |
| Playback methods | `DirectStream`, `Transcode` |
| Events | See the Events table. |
