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
| `(*Controller).Start(ctx) error` | Starts the video and audio worker pools, and the channel pool when `ChannelPoolSize` is above 0. Returns `ErrControllerStopped` if `Stop` already ran. |
| `(*Controller).Stop()` | Idempotent. Stops every live session, cancels detached work, waits up to 10 seconds, releases locks of flights never enqueued. A stopped controller cannot restart. Locks of enqueued jobs expire by TTL. |
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

## Live sessions

`Live` turns a raw MPEG-TS byte stream into a sliding-window HLS playlist. Live sessions need no `Storage` and no `Coordinator`. Segments sit in a temp directory of the process that owns the session.

```go
src := goshl.LiveSource{
	Label: "channel-42",
	Open: func(ctx context.Context) (io.ReadCloser, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstreamURL, nil)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		return res.Body, nil
	},
}
session, err := controller.Live(ctx, "ch42", src, goshl.LiveOptions{})
if err != nil {
	return err
}
if err := session.WaitReady(ctx); err != nil {
	return err
}
playlist, err := session.Playlist(ctx)
```

| Method | Purpose |
|---|---|
| `(*Controller).Live(ctx, key, LiveSource, LiveOptions) (*LiveSession, error)` | Returns at once. A live session for `key` is shared: a second call returns the same `*LiveSession` and ignores its `src` and `opts`. A failed or stopped session is removed, so the next call starts a fresh one. Fails synchronously with `ErrLiveInvalidOptions`, `ErrControllerStopped` or `ErrLiveCapacity`. It can also return the context error, or a generic error when the session directory cannot be created. |
| `(*Controller).LiveProbe(ctx, LiveSource, budget) (LiveProbeInfo, error)` | Opens the source, reads until ffprobe has enough or `budget` passes (0 means 10 s), closes it. Returns the decision a session would make. Takes no slot and starts no session. |

`LiveSession` methods:

| Method | Purpose |
|---|---|
| `WaitReady(ctx) error` | Returns once `ReadySegments` segments exist. Returns the failure of a failed session and `ErrLiveStopped` for a stopped one. |
| `Playlist(ctx) (string, error)` | Waits for readiness, then renders the window. Never contains `EXT-X-ENDLIST`. |
| `Segment(ctx, name) ([]byte, error)` | One segment. `name` is the sequence number: `17`, `17.ts` and `seg-17.ts` are equal. A sequence outside the window plus 4 returns `ErrLiveSegmentNotFound`. |
| `SwitchSource(ctx, LiveSource) error` | Replaces the source without ending the session. The new source is opened and probed while the old one plays. On error the session is unchanged. On success the next segment carries `EXT-X-DISCONTINUITY` and sequence numbers keep rising. `ctx` bounds the wait only. |
| `State() LiveState` | Current status and, when failed, the error. |
| `Info() LiveProbeInfo` | The codec decision of the current source. |
| `Label() string` | The `LiveSource.Label` of the current source. It changes after a successful `SwitchSource`, and later events carry the current label. |
| `Key() string` | The key the session was started with. |
| `Epochs() int` | The number of source epochs: 1 once the first source is adopted, plus one per reconnect and per source switch. Each new epoch is a discontinuity in the playlist. |
| `Done() <-chan struct{}` | Closed when the session ends. |
| `Stop()` | Kills ffmpeg, closes the reader, deletes the files, frees the slot. |

Each touch of `WaitReady`, `Playlist`, `Segment`, a successful `SwitchSource` or a repeated `Live` call resets the idle timer. A failed `SwitchSource` does not.

Segment URIs in the playlist come from `PathGenerator.Segment(key, goshl.LiveRendition, goshl.StreamVideo, sequence)`. `LiveRendition` is `"live"`.

### Live types

| Type | Definition |
|---|---|
| `LiveSource` | `Open func(ctx) (io.ReadCloser, error)`, `Label string`. |
| `LiveOptions` | See the table below. |
| `LiveState` | `Status LiveStatus`, `Err error`. |
| `LiveStatus` | `LiveStarting`, `LiveRunning`, `LiveFailed`, `LiveStopped`. |
| `LiveProbeInfo` | `Video LiveVideoInfo`, `Audios []LiveAudioInfo`, `SelectedAudio int`, `VideoAction`, `AudioAction LiveAction`, `Programs int`, `ProbedBytes int`. |
| `LiveVideoInfo` | `PID`, `Codec`, `Profile`, `PixelFormat`, `Width`, `Height`, `DisplayW`, `FrameRate`, `Interlaced`, `Transfer`, `Primaries`, `Matrix`. |
| `LiveAudioInfo` | `PID`, `Codec`, `Language`, `Channels`, `SampleRate`. |
| `LiveAction` | `LiveCopy`, `LiveTranscode`, `LiveNone`. |

`LiveOptions` is per session. A zero field takes its default.

| Field | Default | Bounds | Meaning |
|---|---|---|---|
| `SegmentDuration` | 4s | 1s to 30s | Target segment length. Copied video cuts at source keyframes, so segments run longer. |
| `WindowSegments` | 6 | 3 to 100 | Segments listed in the playlist. |
| `IdleTimeout` | 60s | at least 1s | Stops the session when nothing touches it for this long. |
| `MaxHeight` | 720 | at least 144 | Height cap when video is transcoded. |
| `PreferredLanguage` | `""` | | Audio language, matched case-insensitively against the TS language tag. |
| `ReadySegments` | 3 | 1 to `WindowSegments` | Segments `WaitReady` waits for. |
| `TranscodeInterlaced` | `false` | | Transcodes interlaced H.264 to progressive H.264 (`bwdif`, capped at `MaxHeight`) instead of copying it. Takes an encoder slot and one unit of `LiveMaxTranscodes`. Browsers cannot decode interlaced H.264 through MSE. |

`LiveState.Status` is `LiveStarting` until `ReadySegments` segments exist, then `LiveRunning`. It stays `LiveRunning` during reconnects. `LiveFailed` carries the error. `LiveStopped` has a nil error.

## Channels

Channels are linear TV built from VOD sources in one fixed output format. The host owns the schedule and the playlist. goshl plans segments, produces them ahead of the playhead and stores them in `Storage`. Requires `Options.ChannelPoolSize` above 0 and a coordinator that implements job contract 6. See [operations.md](operations.md#channels).

```go
profile := goshl.ChannelProfile{}
plan, err := controller.ChannelPlan(ctx, src, profile, 0)
if err != nil {
	return err
}
if err := controller.ChannelPrefetch(ctx, src, profile, 0, 0, 15); err != nil {
	return err
}
data, err := controller.ChannelSegment(ctx, src, profile, 0, 0)
```

| Method | Purpose |
|---|---|
| `ChannelPlan(ctx, sourceURL, ChannelProfile, audioTrack) (ChannelPlan, error)` | The segment list of one item. A pure function of the probed duration and the profile. Enqueues nothing and reads no segment. |
| `ChannelSegment(ctx, sourceURL, ChannelProfile, audioTrack, index) ([]byte, error)` | One segment. A stored segment returns at once. A miss runs the batch that holds it and waits up to `SegmentTimeout`. |
| `ChannelPrefetch(ctx, sourceURL, ChannelProfile, audioTrack, from, count) error` | Enqueues the missing batches for `count` segments from `from` and returns. Never waits for a transcode. Idempotent: overlapping calls enqueue each batch once. `count` of 0 or less returns nil. `from` outside the plan returns `ErrSegmentOutOfRange`. A range past the end is clamped. |

Checks run in this order: `ErrControllerStopped`, `ErrChannelDisabled`, `ErrInvalidChannelProfile`, audio track. `audioTrack` is the 0-based position in `Metadata.Audios`. A source without audio accepts only 0 and carries silence. A track out of range, or a source without video, returns `ErrRenditionNotFound`. `Segment` and `VariantPlaylist` reject rendition names that start with `channel-`.

A failed channel run is remembered for 10 seconds per source and rendition, up to 1024 entries. `ChannelPrefetch` returns that failure during that time. `ChannelSegment` ignores it. `Invalidate` clears it.

### Channel types

| Type | Definition |
|---|---|
| `ChannelProfile` | `Height`, `FrameRateNum`, `FrameRateDen`, `SegmentFrames`, `VideoBitrate`, `AudioBitrate`. `Normalised() (ChannelProfile, error)` fills defaults and validates. |
| `ChannelPlan` | `MediaKey`, `Rendition`, `Codecs`, `FrameRateNum`, `FrameRateDen`, `Frames`, `Duration`, `Segments []Segment`. `SegmentData(index) SegmentData` is the storage key of a segment. |
| `ChannelSpec` | The channel part of a `Job`. See [operations.md](operations.md#coordinator-contract). |
| `JobPriority` | `PriorityBackground` (0), `PriorityPlayback` (100), `PriorityChannel` (200). |

`ChannelCodecs` is `avc1.64001f,mp4a.40.2`. `StreamChannel` is `"channel"`.

| `ChannelProfile` field | Default | Bounds |
|---|---|---|
| `Height` | 720 | 240 to 720, even. Canvas width is `Height` x 16/9, rounded up to even. |
| `FrameRateNum`, `FrameRateDen` | 24000, 1001 | 1 to 60 fps. Reduced to lowest terms. A zero denominator with a set numerator means 1. |
| `SegmentFrames` | 48 | A segment lasts 1 to 10 seconds. |
| `VideoBitrate` | 2500000 | 200000 to 20000000. |
| `AudioBitrate` | 160000 | 32000 to 512000. |

A zero field takes its default. `ChannelProfile{}` and the explicit defaults share one cache.

## Types

### Options

See [configuration.md](configuration.md). `Options` has the fields `Storage`, `Coordinator`, `PathGen`, `HWAccel`, `HWAccelMode`, `SegmentTimeout`, `TargetDuration`, `SegmentsPerJob`, `VideoPoolSize`, `AudioPoolSize`, `DefaultPreviewConfig`, `DefaultThumbnailConfig`, `JobTimeout`, `ProbeTimeout`, `GenerationTimeout`, `SpriteStorage`, `CacheVersion`, `SourceRevision`, `Observer`, `HWAccelDecodeMode`, `AllowHardwareFallback`, `OmitSubtitleRenditions`, `ChannelPoolSize`, `ChannelSegmentsPerJob`, `EncoderSlots`, `LiveMaxSessions` and `LiveMaxTranscodes`.

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
| `StreamType` | `StreamVideo`, `StreamAudio`, `StreamSubtitle`, `StreamChannel`. |
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
| `Job` | One unit of work. Contract 6 adds `Priority JobPriority` and `Channel *ChannelSpec`. See [operations.md](operations.md#coordinator-contract). |
| `JobDelivery` | `Job Job`, `Receipt string`. |
| `JobState` | `JobStateCompleted`, `JobStateFailed`, `JobStateCancelled`. All are terminal. |
| `JobStatus` | `JobID`, `RunID string`, `State JobState`, `Error string`. |
| `VideoRendition` | `Name`, `Width`, `Height`, `Bitrate`, `PeakBitrate`, `BufferSize`, `PeakBandwidth`, `AverageBandwidth`, `FrameRate`, `Codec`, `Level`, `Method`, `Colour`, `StripParameterSets`, `StripSEI`, `SourceEnd`. |
| `AudioRendition` | `Name`, `Codec`, `CodecString`, `Bitrate`, `Channels`, `SampleRate`, `PeakBandwidth`, `AverageBandwidth`, `SourceEnd`, `Method`, `StreamIndex`, `Language`, `Title`, `Default`. |
| `PlaybackMethod` | `DirectStream` (remux, copy) or `Transcode`. |
| `ColourPlan` | `Tonemap`, `Retag`, `AssumeMatrix`, `AssumePrimaries`. Tells the worker how to tag, convert or tone map. |

## Events

`Event` fields: `Type`, `Time`, `MediaKey`, `JobID`, `StreamType`, `Rendition`, `Index`, `StartIndex`, `EndIndex`, `Duration`, `Bytes`, `Accelerator`, `Error`, `Label`. `EventType` is a string. Identities are opaque hashes.

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
| `EventLiveStarted` | `live.started` | A live session was created. |
| `EventLiveReady` | `live.ready` | A live session has `ReadySegments` segments. |
| `EventLiveReconnecting` | `live.reconnecting` | The reader failed and goshl is reopening it. `Error` is a class word. |
| `EventLiveSwitched` | `live.switched` | Every `SwitchSource` that takes effect. |
| `EventLiveFailed` | `live.failed` | A live session failed. `Error` is a class word. |
| `EventLiveStopped` | `live.stopped` | A live session ended. `Error` is `stopped` or `idle`. |

Live events set only `Type`, `Time`, `Label` and, on reconnecting, failed and stopped, `Error`. `Error` is one of `source_unavailable`, `unsupported_codec`, `no_data`, `capacity`, `idle`, `stopped` or `internal`. It never holds the reader's error text. `Label` is the `LiveSource.Label`; keys and URLs are not emitted.

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
| `ErrChannelDisabled` | A channel method was called while `ChannelPoolSize` is 0. |
| `ErrInvalidChannelProfile` | A `ChannelProfile` field is out of range. |
| `ErrLiveInvalidOptions` | `Live` got an empty key, a nil `Open`, or `LiveOptions` out of range. |
| `ErrLiveCapacity` | `Live` hit `LiveMaxSessions`, or a transcode needs a live slot and none is free. From `SwitchSource` it leaves the old source playing. Never retried. |
| `ErrLiveSourceUnavailable` | `Open` failed or returned nil, five reconnects failed, or a source sent valid MPEG-TS and ended inside the probe. Wraps the reader's own error. |
| `ErrLiveNoData` | The source sent nothing, or sent data that is not MPEG-TS. |
| `ErrLiveUnsupportedCodec` | The video is not H.264, HEVC or MPEG-2, or the stream has no video. Never retried, whether it comes from the probe or from building the ffmpeg command. |
| `ErrLiveStopped` | The session was stopped. |
| `ErrLiveSegmentNotFound` | The segment name is unknown or outside the window. |

## Constants

| Group | Constants |
|---|---|
| Stream types | `StreamVideo`, `StreamAudio`, `StreamSubtitle`, `StreamChannel` |
| Accelerators | `AccelNone`, `AccelCUDA`, `AccelVideoToolbox`, `AccelVAAPI`, `AccelQSV` |
| Segment states | `SegmentStateReady`, `SegmentStateError` |
| Job states | `JobStateCompleted`, `JobStateFailed`, `JobStateCancelled` |
| Decode modes | `DecodeSafe`, `DecodeDevice` |
| Playback methods | `DirectStream`, `Transcode` |
| Job priorities | `PriorityBackground`, `PriorityPlayback`, `PriorityChannel` |
| Live status | `LiveStarting`, `LiveRunning`, `LiveFailed`, `LiveStopped` |
| Live actions | `LiveCopy`, `LiveTranscode`, `LiveNone` |
| Live and channel | `LiveRendition`, `ChannelCodecs` |
| Events | See the Events table. |
