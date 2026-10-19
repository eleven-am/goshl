# Goshl Migration Guide

This release changes Goshl's cache identity, distributed job contract, subtitle routing, hardware validation, and media compatibility policy. Applications only need migration when they upgrade to this release.

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

The new release intentionally does not reuse output produced under the old cache identity or media pipeline. Existing objects remain in storage but are cold data from Goshl's perspective.

Storage may implement optional invalidation:

```go
type CacheInvalidator interface {
    DeleteMedia(ctx context.Context, mediaKey string) error
}
```

Generated-asset keys are namespaced beneath the media key so `DeleteMedia` can remove metadata, segments, sprites, subtitles, previews, and thumbnails belonging to that media.

## Coordinator and worker rollout

`Job` is now a concrete, versioned work contract containing:

- Opaque media and pipeline identities.
- A run identity for one execution of a deterministic batch.
- The executable source URL.
- Exact segment boundaries and expected output indexes.
- Concrete video or audio rendition parameters.
- Enqueue time and execution deadline.
- Direct-stream overlap information when required.

Coordinators must serialize and restore every field without recalculating it. Enqueue must be idempotent by `Job.RunID`.

Worker subscriptions now include the pipeline profile:

```go
deliveries, err := coordinator.Subscribe(ctx, goshl.StreamVideo, pipelineProfile)
```

The coordinator must route a job only to a subscriber whose stream type and profile both match the job. This prevents a software, CUDA, VideoToolbox, VAAPI, or QSV worker from consuming work produced for another pipeline.

Job lifecycle is run scoped:

```go
status, err := coordinator.WaitJob(ctx, job.RunID)
err = coordinator.SettleJob(ctx, delivery.Receipt, goshl.JobStatus{
    JobID: job.ID,
    RunID: job.RunID,
    State: goshl.JobStateCompleted,
})
```

`WaitJob` must fan out status changes to every waiter and replay the latest state to late subscribers. `SettleJob` must atomically validate the active receipt, retain terminal state until at least one minute after `Job.Deadline`, and remove the delivery. The receipt-to-status settlement record must be retained for the same period so repeating the same settlement remains idempotent. Terminal states are `completed`, `failed`, and `cancelled`. Waiting subscribers must be removed when their context ends.

Terminal states are immutable and must not be replaced.

Job delivery now returns `JobDelivery`, which contains the job and an opaque receipt. Coordinators must give each delivery an initial lease of at least five seconds, renew only the specified receipt through `Renew`, atomically finish valid work through `SettleJob`, and redeliver after an unrenewed lease expires. `Ack` only discards malformed deliveries that cannot publish valid run status. Receipts must never be shared across deliveries or runs, and redelivery must invalidate the previous receipt.

`AcquireJob` must atomically acquire the deterministic batch lock and register the proposed run ID. If the lock is held, it must return `ErrLockHeld` and the active run ID. `ReleaseJob` must clear the mapping only when the job ID, run ID, and token all match. Segment status is only a transient readiness hint and must not be replayed. Failures are delivered through run scoped job status.

Goshl no longer cancels work when the last waiter on one process leaves. Local waiter counts cannot prove that no other replica needs the same batch. Hosts that need abandonment cancellation must implement distributed waiter leases before requesting cancellation.

Workers reject jobs produced for a different pipeline profile. Upgrade producers and workers together, and drain or remove old queued jobs before directing traffic to the new version. Do not run old and new Goshl workers against the same active queue during migration.

`Coordinator.Close` is no longer part of Goshl's required interface. The application remains responsible for the coordinator's lifecycle.

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

No interface changes are required for indexed subtitle extraction. Progressive MP4 sources with `mov_text`/`tx3g` or `wvtt` tracks now use local random access or HTTP range requests when a subtitle is first requested. The result is written with the existing `Storage.WriteSubtitleVTT` method and reused normally. Remote media endpoints should support single byte ranges and return `206 Partial Content` with an accurate `Content-Range`; sources without that support use the existing FFmpeg path.

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

## NVIDIA and other hardware accelerators

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

## Migration checklist

- Update all `SegmentData.SourceURL` references to `SegmentData.MediaKey`.
- Store opaque media and asset keys exactly as supplied.
- Add `SubtitlePlaylist` routing and use subtitle track IDs.
- Update coordinator job serialization for every current `Job` field.
- Route `Subscribe` by both stream type and pipeline profile.
- Implement durable, replayable `WaitJob` and atomic `SettleJob` lifecycle state.
- Implement atomic active-run registration through `AcquireJob` and `ReleaseJob`.
- Deliver jobs with unique receipts and implement receipt-scoped `Renew`, `SettleJob`, and malformed-delivery `Ack`.
- Drain old jobs and deploy all producers and workers on the same Goshl version.
- Configure `SourceRevision` for remote or signed sources.
- Decide between strict hardware failure and `AllowHardwareFallback`.
- Account for a cold cache and remove old cache data when operationally safe.
- Compile every consumer against the new package before upgrading it.
- Run a real playback and seek test in each target GPU environment.
