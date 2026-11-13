# Goshl Migration Guide

This guide covers everything that changes for an application upgrading to this release: cache identity, the distributed job contract, runtime requirements, output timeline, hardware validation, and removed API. Applications only need to migrate when they upgrade.

## Summary

- Cache schema is now `goshl-cache-v4`; every source is re-probed and re-transcoded once.
- Job contract version is 4. Jobs gain `StartBy` (replacing `Deadline`), `Trailing` and `VideoStreamIndex`; external Coordinators must carry them. Producers and workers must roll together.
- FFmpeg and FFprobe 6.1 or newer are required.
- Output media sits on a timeline offset by 10 seconds; subtitle WebVTT carries an `X-TIMESTAMP-MAP`.
- Sources must be local paths, `file:` URLs or `http(s)` URLs; every other scheme is rejected.
- Several never-functional public symbols were removed (see Removed API).

## Runtime requirements

- `ffmpeg` and `ffprobe` must be version 6.1 or newer. Transcoded video uses `-enc_time_base demux`, which older releases lack; the constructor now rejects older builds at startup with an error wrapping `ErrInvalidOptions` instead of failing video jobs at runtime.
- Every FFmpeg and FFprobe invocation runs with the protocol whitelist `file,http,https,tcp,tls`. `tcp` and `tls` are whitelisted only for FFmpeg's own use beneath `http(s)`. Sources with any other scheme, including `tcp://` and `tls://` and `rtmp://`, `ftp://`, `concat:`, `data:`, are rejected. Local paths, `file:` URLs and `http(s)` URLs, including paths containing spaces, colons or a leading dash, keep working.

## Output timeline

Generated video and audio segments now start at 10 seconds on the output timeline instead of 0, so every rendition shares one continuous, gap-free clock across segment boundaries (copied-video jobs may carry an extra trailing segment for this reason; see the Job contract). Players need no change.

Subtitle WebVTT files now begin with `X-TIMESTAMP-MAP=MPEGTS:900000,LOCAL:00:00:00.000` so cues stay aligned with the offset video. If you post-process stored or served subtitle files, keep that header line. Sprite, preview and thumbnail output is not shifted.

The offset is part of the pipeline profile; previously stored segments and subtitles are never reused.

## Source URLs and media keys

Continue passing local paths, NFS-mounted paths, HTTP URLs, or HTTPS URLs to controller methods:

```go
segment, err := controller.Segment(
    ctx,
    "https://media.example/video.mkv?token=secret",
    goshl.StreamVideo,
    "720p",
    10,
)
```

The original source remains available to transcoding workers as `Job.SourceURL`. FFmpeg and FFprobe use this value as their input.

`SegmentData.MediaKey` is now the canonical storage and notification identity. It is an opaque, versioned hash and must never be opened as a URL or file:

```go
func (s *Storage) WriteSegment(ctx context.Context, info goshl.SegmentData, data []byte) error {
    objectKey := fmt.Sprintf(
        "%s/%s/%t/%d.ts",
        info.MediaKey,
        info.Rendition,
        info.IsVideo,
        info.Index,
    )
    return s.write(ctx, objectKey, data)
}
```

Storage implementations must treat metadata and generated-asset string keys exactly as supplied. Do not reconstruct them from a source URL.

## Remote source revisions

Local files use their size and modification time as the default source revision. Remote URLs should provide a stable ETag, object version, or content fingerprint:

```go
controller, err := goshl.NewControllerWithContext(ctx, goshl.Options{
    Storage:     storage,
    Coordinator: coordinator,
    PathGen:     pathGenerator,
    SourceRevision: func(ctx context.Context, sourceURL string) (string, error) {
        return mediaVersion(ctx, sourceURL)
    },
})
```

Without `SourceRevision`, the complete remote URL determines identity. A refreshed signed URL will therefore create a new cache namespace even when it points to the same media.

## Cache migration

The cache schema is now `goshl-cache-v4`, and the pipeline profile also covers the output timeline offset and the job contract version. Output produced under the previous schema (v3) is not reused. After deploying, every source is re-probed and re-transcoded, and every subtitle, sprite, preview and thumbnail is regenerated. Plan for the one-time probe and transcoding load, and remove cold v3 data when operationally safe. Existing objects remain in storage but are cold data from Goshl's perspective.

Storage may implement optional invalidation:

```go
type CacheInvalidator interface {
    DeleteMedia(ctx context.Context, mediaKey string) error
}
```

Generated-asset keys are namespaced beneath the media key so `DeleteMedia` can remove metadata, segments, sprites, subtitles, previews, and thumbnails belonging to that media. If you configure `Options.SpriteStorage`, it must implement `CacheInvalidator` too, otherwise `Invalidate` returns `ErrCacheInvalidationUnsupported` before deleting anything.

## Coordinator and worker contract

`Job` is a concrete, versioned work contract. Coordinators must serialize and restore every field without recalculating or dropping any, including fields they do not interpret:

- `ID`, `RunID`, `MediaKey`, `Profile`: batch identity, execution identity, opaque media and pipeline identities.
- `SourceURL`: the executable source; FFmpeg and FFprobe use it as their input.
- `Rendition`, `StreamType`, `StartIndex`, `EndIndex`, `RequestedIndex`, `ExpectedIndices`, `Segments`: exact segment boundaries and expected output indexes.
- `VideoRendition`, `AudioRendition`: concrete rendition parameters.
- `EnqueuedAt` and `StartBy`: see below.
- `Overlap`: a preceding range needed so direct-stream and audio segments join cleanly.
- `Trailing`: for copied video, one extra segment after the last requested one, produced and discarded so the final requested segment ends on the correct frame. The worker consumes it; the coordinator only carries it.
- `VideoStreamIndex`: the source video stream the worker must encode, so a worker need not re-probe. (Earlier development builds carried a full `Metadata` copy on the job; that field no longer exists.)

`Job.Deadline` was renamed to `StartBy` and its meaning changed. `StartBy` is the latest moment a worker may begin the job. A worker refuses a delivery once `StartBy` has passed and otherwise runs it with a full `JobTimeout` measured from the moment execution starts, so time spent queued no longer shortens the execution budget. The requesting replica sets `StartBy` to enqueue time plus `JobTimeout`, and keeps its in-process flight until `StartBy` plus `JobTimeout`. The distributed job lock lives one minute longer.

The job contract version is 4. It is part of the pipeline profile hash, so replicas running a different contract never exchange jobs (see Rolling deploy).

`Enqueue` is idempotent by `Job.RunID`. It must return an error wrapping `goshl.ErrEnqueueRejected` only when the job was definitely not persisted. Goshl then releases the segment flight and job lock at once so the next request can enqueue again. Any other `Enqueue` error is treated as ambiguous (the job may exist), so the run and lock stay registered until a terminal status or the lock TTL.

**Required for external coordinators:** wrap `ErrEnqueueRejected` in every `Enqueue` error that definitely did not persist the job (validation failure, connection refused before send, and the like). A coordinator that never does so still works, but after any failed `Enqueue` that batch cannot be retried until the lock TTL (`StartBy` plus `JobTimeout` plus one minute) expires.

Worker subscriptions include the pipeline profile:

```go
deliveries, err := coordinator.Subscribe(ctx, goshl.StreamVideo, pipelineProfile)
```

The coordinator must route a job only to a subscriber whose stream type and profile both match the job. This prevents a software, CUDA, VideoToolbox, VAAPI, or QSV worker from consuming work produced for another pipeline.

Delivery and lifecycle:

- `Subscribe` returns `JobDelivery` values containing the job and an opaque receipt. Each delivery gets a unique receipt and an initial lease of at least five seconds. `Renew` extends only the given receipt; an unrenewed delivery is redelivered after the lease expires, and redelivery invalidates the previous receipt.
- `SettleJob` atomically validates the active receipt, persists the terminal status and removes the delivery. Retrying with the same receipt and status must be idempotent. Both the terminal status and the receipt-to-status record must be retained until at least one minute after `StartBy` plus `JobTimeout`.
- `Ack` only discards malformed deliveries that cannot publish a valid run status.
- Job status is keyed by `RunID`. `WaitJob` fans out status changes to every waiter and replays the latest state to late subscribers. Terminal states are `completed`, `failed` and `cancelled`, and are immutable.
- `AcquireJob` atomically acquires the deterministic batch lock and registers the proposed run ID. If the lock is held it returns `ErrLockHeld` and the active run ID. `ReleaseJob` clears the mapping only when job ID, run ID and token all match.
- Segment status (`NotifySegment`/`WaitSegment`) is a transient readiness hint and must not replay. Failures travel through run-scoped job status. Waiting subscribers must be removed when their context ends.
- Goshl does not cancel work when the last waiter on one process leaves; local waiter counts cannot prove that no other replica needs the batch. Hosts that need abandonment cancellation must implement distributed waiter leases themselves.
- `Coordinator.Close` is no longer part of the required interface. The application owns the coordinator's lifecycle.

### Rolling deploy

Pipeline profiles include the cache schema, job contract version, output timeline offset and active accelerator. Replicas on different releases therefore subscribe to different queues and never exchange jobs, but a requester of the old release cannot be served by workers of the new one.

1. Update your Coordinator implementation first so it carries every `Job` field (`StartBy`, `Trailing`, `VideoStreamIndex`). It is safe to deploy ahead of Goshl.
2. Stop traffic or drain queued jobs of the old release; do not leave old jobs in queues the new workers will read.
3. Upgrade producers and workers together.
4. Expect a cold cache (see Cache migration), then remove v3 data.

Mixed-hardware replicas (for example CUDA and software) also use separate profiles and queues; this is intentional.

## Path generation and subtitles

`PathGenerator` contains only routes embedded into generated playlists:

```go
type PathGenerator interface {
    VariantPlaylist(sourceURL string, rendition string, streamType goshl.StreamType) string
    Segment(sourceURL string, rendition string, streamType goshl.StreamType, index int) string
    Sprite(sourceURL string, index int) string
    SubtitlePlaylist(sourceURL string, trackID string) string
    SubtitleVTT(sourceURL string, trackID string) string
}
```

Subtitle selection now uses stable track IDs such as `subtitle-3`, not language alone. This allows multiple tracks with the same or missing language tag.

Expose handlers for both methods:

```go
playlist, err := controller.SubtitlePlaylist(ctx, sourceURL, "subtitle-3")
webVTT, err := controller.SubtitleVTT(ctx, sourceURL, "subtitle-3")
```

No interface changes are required for indexed subtitle extraction. Progressive MP4 sources with `mov_text`/`tx3g` or `wvtt` tracks now use local random access or HTTP range requests when a subtitle is first requested. The result is written with `Storage.WriteSubtitleVTT` and reused normally. Remote media endpoints should support single byte ranges and return `206 Partial Content` with an accurate `Content-Range`; sources without that support use the existing FFmpeg path.

## Controller construction

Use the error-returning constructor in applications:

```go
controller, err := goshl.NewControllerWithContext(ctx, options)
if err != nil {
    return err
}
```

`NewController` remains available as a panic-on-error compatibility wrapper.

Invalid durations, pool sizes, batch sizes, preview settings, thumbnail settings, stream types, renditions, and segment indexes now fail explicitly instead of reaching FFmpeg or waiting for a timeout.

## Hardware accelerators

Hardware selection now runs a bounded functional decode, filter, and encode test.

CUDA defaults to the safe host-frame path:

```go
HWAccelMode:       goshl.AccelCUDA,
HWAccelDecodeMode: goshl.DecodeSafe,
```

This keeps NVIDIA decoding and encoding enabled without retaining decoded frames in the CUDA surface pool. It is the compatible path for the Blackwell `cuvidCreateDecoder` and `No decoder surfaces left` failures reproduced in the Goshl lab.

Full device-frame decoding is explicit:

```go
HWAccelMode:       goshl.AccelCUDA,
HWAccelDecodeMode: goshl.DecodeDevice,
```

Explicit hardware selection returns a construction error when validation fails. Set `AllowHardwareFallback: true` only when falling back to software is acceptable. Auto-detection already falls back and records the reason in `HWAccelConfig()` and observer events.

## Playback behavior

Direct H.264 remuxing is now limited to inputs that are safe for browser HLS:

- Baseline, Main, or High profile.
- Known supported H.264 level.
- 8-bit `yuv420p` or `yuvj420p`.
- A random-access frame at the beginning.
- No unresolved rotation or anamorphic sample aspect ratio.

Other H.264 inputs transcode to deterministic High-profile `yuv420p` output. This may increase transcoding load for media that older Goshl versions copied directly.

Master playlists now distinguish audio codec and channel groups, include aggregate bandwidth and codec declarations, support silent and audio-only media, and expose subtitle groups.

## Operational behavior

- Segment locks use the canonical batch identity across replicas.
- Job status uses the run identity so a new execution cannot inherit stale terminal state.
- Workers receive jobs only from queues matching their pipeline profile.
- Remote terminal states close the originating flight and release its distributed lock.
- Local waiter departure does not cancel shared cluster work.
- Waiting requests recheck durable storage and tolerate missed or closed notification channels.
- Stored segments remain successful when notification or acknowledgement delivery fails.
- Jobs have hard execution deadlines.
- Partial, malformed, duplicate, unexpected, and empty FFmpeg outputs fail the job.
- Preview memory is bounded by requested frame count.
- Thumbnails seek directly instead of generating every sprite first.
- FFmpeg and FFprobe output is bounded and media credentials are redacted from returned errors.

## Sprites and thumbnails

- The sprite cache identity comes from the generator's own sprite settings (`160x90:5:10x10` by default). If the sprite settings ever change, the identity changes with them and sheets regenerate once, leaving the old ones orphaned in `SpriteStorage` until `Invalidate` or your own cleanup removes them. (Under cache v4 all sprites regenerate once anyway.)
- The sprite VTT is no longer stored. `SpriteVTT` ensures the sprite sheets exist and renders the VTT on every request using the current `PathGenerator.Sprite`, so changing sprite routes (for example signed URLs) takes effect immediately. VTTs stored by earlier versions are ignored and can be deleted.
- `Invalidate` also purges `Options.SpriteStorage` when configured (see Cache migration).
- A zero `ThumbnailConfig.Position` means "use the default position" in both `Options.DefaultThumbnailConfig` and per-request configs, consistent with `Width`. A request with only `Width` set no longer thumbnails at position 0. Position 0.0 (the very first frame) is therefore not selectable; use a small positive value such as 0.001.

## Lifecycle and observability

- `Controller.Stop` is now final. After it, every method except `Stop`, `HWAccelConfig` and the constructors returns the new `ErrControllerStopped`. `Stop` cancels detached work (asset generation, metadata probes, flight watchers), waits up to 10 seconds, and releases the locks of flights that were never enqueued. Locks of already-enqueued jobs expire by TTL. Build a new controller instead of restarting.
- Metadata probe failures are cached in process for 10 seconds (up to 1024 sources); `Invalidate` for a source forgets its entry. A probe that exceeds `ProbeTimeout`, on this replica or the one holding the lock, surfaces as `ErrMetadataTimeout` (which also wraps `context.DeadlineExceeded`).
- New events: `EventPoolSubscriptionLost` (a worker pool's coordinator subscription ended) and `EventJobDeliveryRejected` (a malformed or wrong-profile delivery was refused, including when acknowledging it also failed). A delivery whose `StartBy` passed is settled failed with a start-by error and emits `EventJobFailed`.
- `EventLockWait` is also emitted for metadata and generated-asset locks.

## Removed API

- `Options.AbandonGrace` was never honored and has been removed.
- `EventJobAbandoned` and `EventSegmentFailed` were never emitted and have been removed.
- `WriteSpriteVTT`, `ReadSpriteVTT` and `SpriteVTTExists` are removed from `Storage` and `SpriteStorage`. Implementations may keep the methods; nothing calls them.
- `Job.Deadline` is replaced by `Job.StartBy` (new semantics, see above).
- `Coordinator.Close` and unused route methods are no longer part of the required interfaces.
- `SegmentData.SourceURL` is replaced by `SegmentData.MediaKey`.

## Migration checklist

- Install ffmpeg and ffprobe 6.1 or newer on every host; confirm sources use only supported schemes.
- Update all `SegmentData.SourceURL` references to `SegmentData.MediaKey`, and store opaque media and asset keys exactly as supplied.
- Remove any use of the removed API above; compile every consumer against the new package before upgrading it.
- Add `SubtitlePlaylist` routing and use subtitle track IDs.
- Update coordinator job serialization for every current `Job` field, including `StartBy`, `Trailing` and `VideoStreamIndex`, and rename `Deadline` to `StartBy`.
- Required: wrap `goshl.ErrEnqueueRejected` in `Enqueue` errors that definitely did not persist the job; otherwise failed enqueues block retries until the lock TTL.
- Route `Subscribe` by both stream type and pipeline profile, with unique receipts and receipt-scoped `Renew`, `SettleJob` and malformed-delivery `Ack`.
- Implement durable, replayable `WaitJob` and atomic `SettleJob`, retaining status for at least one minute past `StartBy` plus `JobTimeout`.
- Implement atomic `AcquireJob` and `ReleaseJob`.
- Drain old jobs and deploy all producers and workers on the same Goshl version.
- Keep the `X-TIMESTAMP-MAP` header if you transform subtitle files.
- Configure `SourceRevision` for remote or signed sources.
- Decide between strict hardware failure and `AllowHardwareFallback`.
- Account for a cold cache and remove old cache data when operationally safe.
- Run a real playback and seek test in each target GPU environment.
