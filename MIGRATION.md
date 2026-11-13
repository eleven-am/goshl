# Goshl Migration Guide

This guide covers upgrading to the current release: cache schema `goshl-cache-v5` and job contract version 5. The last published version is v0.0.22, which predates both this release and the v4 release before it, so the guide has two parts. Read both when upgrading from v0.0.22. If you already run the v4 release, read only the first part.

## Summary

- Cache schema is now `goshl-cache-v5` (v0.0.22 used v3). Every source is re-probed and re-transcoded once, and every subtitle, sprite, preview and thumbnail is regenerated.
- Job contract version is 5. A coordinator must round-trip every `Job` field and every `VideoRendition` and `AudioRendition` field; see "Coordinator and worker contract". Producers and workers must roll together.
- Segment counts and indices change for most sources (new planner).
- FFmpeg and FFprobe 6.1 or newer are required, enforced at startup. HDR sources need an ffmpeg with `zscale` and `tonemap`.
- Output media sits on a timeline offset by 10 seconds; subtitle WebVTT carries an `X-TIMESTAMP-MAP`.
- Sources must be local paths, `file:` URLs or `http(s)` URLs; every other scheme is rejected.
- Subtitle tracks that cannot be converted to WebVTT are no longer served or listed.
- Several never-functional public symbols were removed (see Removed API).

## Part 1: Changes in this release (cache v5, job contract v5)

### Cache and pipeline profile

The cache schema moves from `goshl-cache-v4` to `goshl-cache-v5`, and the job contract version from 4 to 5. Both are part of the pipeline profile, so v4 and v5 replicas never exchange jobs. Expect a cold cache: all metadata, segments, subtitles and generated assets are produced again. Old objects remain in storage as cold data; remove them when operationally safe (see "Cache migration" in Part 2).

### Coordinator and worker contract

A coordinator must serialize and restore every field below without rounding, recalculating or dropping any, including fields it does not interpret. Workers act on these fields directly, so a coordinator that drops one changes the output rather than failing with a clear error.

`Job`:

- `ID`, `RunID`, `MediaKey`, `Profile`: batch identity, execution identity, opaque media and pipeline identities.
- `SourceURL`: the executable source.
- `Rendition`, `StreamType`, `StartIndex`, `EndIndex`, `RequestedIndex`, `ExpectedIndices`: which segments the job produces.
- `Segments`: exact boundaries (`Index`, `Start`, `End`, `Duration`, all float64 seconds). Do not round them: adjacency between segments is compared with a tolerance of 1e-6 seconds.
- `EnqueuedAt`, `StartBy`: `StartBy` is the latest moment a worker may begin the job.
- `Overlap`: the segment before the first one. Set for audio jobs and for copied-video jobs that do not start at segment 0. A coordinator that drops it breaks the join between jobs.
- `Trailing`: one segment after the last one, for copied video only.
- `VideoStreamIndex`: the source video stream the worker encodes.
- `VideoRendition`, `AudioRendition`: see below.

`VideoRendition`:

- `Name`, `Width`, `Height`, `Bitrate`, `PeakBitrate`, `FrameRate`, `Codec`, `Level`, `Method`.
- `BufferSize`: rate-control buffer, derived from the H.264 level.
- `Colour`: a `ColourPlan` (`Tonemap`, `AssumeMatrix`, `AssumePrimaries`) telling the worker how to tag, convert or tone map the source.
- `SourceEnd`: the video end in seconds when the video ends more than 0.5 s before the end of the file, otherwise 0. The worker repeats the last frame to fill the remaining segments.

`AudioRendition`:

- `Name`, `Codec`, `CodecString`, `GroupID`, `Bitrate`, `Channels`, `Method`, `StreamIndex`, `Language`, `Default`.
- `Title`: the track title, used for the playlist `NAME`.
- `SampleRate`: the source sample rate. Workers use it to join audio frame-exactly across jobs for 44.1 kHz and 32 kHz sources.
- `SourceEnd`: the end of the source audio stream in seconds, 0 if unknown. Workers pad the audio with silence (`apad`) up to the plan.

A JSON or protobuf coordinator written against v4 almost certainly drops `BufferSize`, `Colour`, `SourceEnd` (both renditions), `SampleRate` and `Title`. Add them before deploying. Adding fields ahead of the Goshl upgrade is safe.

### Rolling deploy

1. Update your Coordinator implementation first so it carries every field above. This is safe to deploy ahead of Goshl.
2. Stop traffic or drain queued jobs of the old release. Do not leave old jobs in queues the new workers read; v5 workers subscribe to a different profile and will not take them.
3. Upgrade producers and workers together.
4. Expect a cold cache, then remove cold data.

### Runtime requirements

- `ffmpeg` and `ffprobe` 6.1 or newer. The constructor runs both at startup and returns an error wrapping `ErrInvalidOptions` for an older or unrecognised version. Development builds are accepted.
- For HDR (PQ or HLG) sources, an `ffmpeg` with the `zscale` (libzimg) and `tonemap` filters, for example Homebrew `ffmpeg-full` or `jellyfin-ffmpeg`, the same build on every replica. It is detected on first use, not at startup. Without it, the HDR renditions of that source fail explicitly. SDR sources are unaffected.
- The trust model for source URLs is documented in the README: goshl does not restrict hosts, its own range reader follows at most 5 redirects, and FFmpeg follows redirects under its own limits. Inputs are now restricted to container formats with `-format_whitelist`, so a source that is an HLS or ffconcat playlist is rejected.

### Segment plan

Segments last at most `max(2 x target, 12 s)`. A long keyframe gap is split into equal pieces, the planner cuts early at the previous keyframe to stay under the cap, and a tail sliver shorter than `min(0.5 s, target / 2)` merges into the previous segment. Cuts tolerate rounding noise, so millisecond-rounded MKV keyframes no longer merge segments. Segment counts and indices therefore change for most sources, which is part of why the cache schema changed.

Direct stream is lost when a source keyframe gap exceeds the cap (12 s at the default target). Such sources transcode their source-resolution rung. Direct stream is also not offered for 8-bit H.264 tagged PQ or HLG, and for a video that ends more than 0.5 s before the end of the file.

### Probe and metadata

- `Metadata` gains `Bitrate` (container bits per second, 0 = unknown). `VideoStream` and `AudioStream` gain `End` (seconds on the `Metadata` timeline, 0 = unknown).
- `End` is measured from the last packets when the container declares nothing: one bounded read of the file tail at probe time. It stays 0 only when no packet is found within 15 minutes of the end, or the read takes over 20 seconds.
- `VideoStream.FrameRate` is now the average frame rate when known, so VFR and field-rate sources change (50 becomes 25).
- `Duration` is bounded by known stream ends. A missing container duration is derived from the streams (audio included) or from the packet scan.
- A real I/O error reading the MP4 header (local or remote) now fails the probe instead of falling back to the packet scan. Remote MP4s are indexed from the `moov` box.
- MKV and WebM keyframes come from the Matroska Cues element instead of a full packet scan, so remote MKVs need no full download. If a cue list has a gap over 11.5 s, the probe falls back to the scan. `VideoStream.End` on cued MKVs comes from the `DURATION` tag, or is 0 when the tag is absent.

### Video and audio output

- Software transcodes use libx264 `superfast` instead of `ultrafast`. Output is High profile as advertised (previously Constrained Baseline). Expect about 1.5 times the CPU per software video job.
- All transcodes are tagged BT.709 limited range. Untagged sources are assumed BT.709 (1280 pixels wide or more, or over 576 lines) or SMPTE 170M. Full-range sources are converted instead of being emitted as `yuvj420p`. HDR sources are tone mapped with `zscale` and `tonemap`.
- Transcoded audio from sources that are not 48 kHz (44.1 kHz, 32 kHz) now joins frame-exactly across jobs. Previously every job after the first started late and long sources failed.
- Sources whose video starts after the container start are cut on the plan. Every job's output is validated against its expected segment times, and a mismatch fails the job instead of caching misplaced segments.
- Direct-stream eligibility now compares the first keyframe with the video stream's own start instead of zero, so H.264 sources whose video starts slightly after the container start (for example an MP4 edit list of 0.083 s) are copied instead of transcoded.
- A video that ends more than 0.5 s before the end of the file no longer fails its last segments: the remaining segments repeat the last frame, and no copied source rung is offered. `VideoRendition.SourceEnd` carries this.
- Each TS segment now sets the MPEG-TS discontinuity indicator on its first packet (`mpegts_flags=+initial_discontinuity`).
- Video renditions that would need an H.264 level above 5.2 are left out of the ladder, and the new `EventRenditionDropped` reports each one.

### Hardware acceleration

- Auto-detection validates every detected accelerator in the order CUDA, QSV, VideoToolbox, VAAPI and keeps the first that passes. `HWAccelConfig().Error` and `EventHardwareFallback` can now be non-empty while a hardware accelerator is selected, because they list the accelerators that were skipped.
- Constructor worst case with `HWAccel: true` rises from 20 s to 70 s (10 s detection plus 15 s for each of four accelerators). Pass a context with a deadline to `NewControllerWithContext` to cap it.
- VideoToolbox output no longer carries a keyframe every 12 frames (`-g 250`).

### Playlists

- Audio groups follow RFC 8216: each group has the same members, one per source track. `NAME` is the track title or language label (previously the rendition name), with exactly one `DEFAULT` per group. `LANGUAGE` is an RFC 5646 tag (`en`, not `eng`).
- Subtitle `DEFAULT` follows the source disposition, `NAME` is unique, and `FORCED=YES` implies `AUTOSELECT=YES`.
- An audio-only master carries `BANDWIDTH` and `SUBTITLES`.
- `MasterPlaylist`, `SubtitlePlaylist` and `VariantPlaylist` return errors instead of writing invalid playlists (no renditions, zero bitrate, empty codec, zero duration or no segments).

### Subtitles

- Only text subtitle codecs are advertised and served (allowlist in `internal/subtitle/codec.go`). Bitmap tracks (PGS, DVD, DVB, XSUB, teletext) and unlisted codecs such as TTML are no longer in the master playlist. `SubtitlePlaylist`, `SubtitleVTT` and `SubtitlesVTT` return an error wrapping `ErrRenditionNotFound` for them and for unknown track IDs. The error text for unknown IDs changes from `subtitle track X not found` to `rendition not found: subtitle track X`. `Metadata` still lists every track.
- A failed single-track batch is no longer rerun with the identical command. FFmpeg runs with `-y` and `-nostdin`, retries write fresh files, and a partial batch output is never stored.
- Subtitle tracks of MKV sources whose cues cover them are read from only the cued blocks and converted by ffmpeg as before, so output is byte-identical. Other tracks fall back to one full pass shared by all requested tracks.

### Runtime and lifecycle

- `Start` returns `ErrControllerStopped` when `Stop` has already run.
- Local joiners keep waiting on the local flight while another replica holds the job lock, and retry after a failed remote job. A client leaving mid-enqueue no longer orphans the job lock. Lock-held waiters return `ErrControllerStopped` on `Stop`.
- A pool whose `Start` context is cancelled with a cause leaves running jobs for redelivery instead of settling them failed.
- Options are bounded: `TargetDuration` 1-60 s, `SegmentsPerJob` at most 1000, pool sizes at most 1024. Violations wrap `ErrInvalidOptions`.

### New and changed API

- New event `EventRenditionDropped` (`Rendition` names the rung, `Error` gives the reason). Emitted on each `MasterPlaylist` call.
- `Metadata.Bitrate`, `VideoStream.End`, `AudioStream.End`, `VideoRendition.SourceEnd`, `VideoRendition.BufferSize`, `VideoRendition.Colour`, `AudioRendition.SampleRate`, `AudioRendition.SourceEnd`, `AudioRendition.Title` are new fields.
- `ErrUnsupportedSource` is returned for an empty source, a scheme other than file, http or https, or a malformed URL.

### Checklist for this release

- Make your Coordinator carry every field in "Coordinator and worker contract", then deploy producers and workers together on the same version.
- Install ffmpeg and ffprobe 6.1 or newer everywhere; if you serve HDR media, use a build with `zscale` and `tonemap` on every replica.
- Re-read the trust model in the README and validate user-supplied source URLs in your application.
- Budget for a cold cache and roughly 1.5 times the software video CPU.
- Subscribe to `EventRenditionDropped` if you want to know when a rung is omitted.
- Review any handling of subtitle errors: unconvertible and unknown tracks now return `ErrRenditionNotFound`.
- Verify in Safari: playback across segment boundaries, the discontinuity indicator, and sources whose video starts slightly after the container start.
- Run a real playback and seek test in each target GPU environment.

## Part 2: Changes since v0.0.22 introduced in the v4 release

The material below was introduced in the v4 release (cache v4, job contract 4). It still applies when upgrading from v0.0.22. Where it names version numbers, v5 supersedes them: read "v4" as "v5" for the cache schema and job contract.

### Protocol whitelist and FFmpeg version

- `ffmpeg` and `ffprobe` must be version 6.1 or newer. Transcoded video uses `-enc_time_base demux`, which older releases lack; the constructor now rejects older builds at startup with an error wrapping `ErrInvalidOptions` instead of failing video jobs at runtime.
- Every FFmpeg and FFprobe invocation runs with the protocol whitelist `file,http,https,tcp,tls`. `tcp` and `tls` are whitelisted only for FFmpeg's own use beneath `http(s)`. Sources with any other scheme, including `tcp://` and `tls://` and `rtmp://`, `ftp://`, `concat:`, `data:`, are rejected. Local paths, `file:` URLs and `http(s)` URLs, including paths containing spaces, colons or a leading dash, keep working.

### Output timeline

Generated video and audio segments now start at 10 seconds on the output timeline instead of 0, so every rendition shares one continuous, gap-free clock across segment boundaries (copied-video jobs may carry an extra trailing segment for this reason; see "Coordinator and worker contract" in Part 1). Players need no change.

Subtitle WebVTT files now begin with `X-TIMESTAMP-MAP=MPEGTS:900000,LOCAL:00:00:00.000` so cues stay aligned with the offset video. If you post-process stored or served subtitle files, keep that header line. Sprite, preview and thumbnail output is not shifted.

The offset is part of the pipeline profile; previously stored segments and subtitles are never reused.

### Source URLs and media keys

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

### Remote source revisions

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

### Cache migration

The cache schema is `goshl-cache-v5` (v4 in the v4 release), and the pipeline profile also covers the output timeline offset and the job contract version. Output produced under the previous schemas (v3 in v0.0.22, v4) is not reused. After deploying, every source is re-probed and re-transcoded, and every subtitle, sprite, preview and thumbnail is regenerated. Plan for the one-time probe and transcoding load, and remove cold v3 and v4 data when operationally safe. Existing objects remain in storage but are cold data from Goshl's perspective.

Storage may implement optional invalidation:

```go
type CacheInvalidator interface {
    DeleteMedia(ctx context.Context, mediaKey string) error
}
```

Generated-asset keys are namespaced beneath the media key so `DeleteMedia` can remove metadata, segments, sprites, subtitles, previews, and thumbnails belonging to that media. If you configure `Options.SpriteStorage`, it must implement `CacheInvalidator` too, otherwise `Invalidate` returns `ErrCacheInvalidationUnsupported` before deleting anything.

### Delivery, enqueue and lifecycle

`Job` is a concrete, versioned work contract. The full list of `Job`, `VideoRendition` and `AudioRendition` fields a coordinator must round-trip is in "Coordinator and worker contract" in Part 1. The v4 additions were `StartBy` (replacing `Deadline`), `Trailing` and `VideoStreamIndex`. `Trailing` is, for copied video, one extra segment after the last requested one, produced and discarded so the final requested segment ends on the correct frame. Earlier development builds carried a full `Metadata` copy on the job; that field no longer exists.

`Job.Deadline` was renamed to `StartBy` and its meaning changed. `StartBy` is the latest moment a worker may begin the job. A worker refuses a delivery once `StartBy` has passed and otherwise runs it with a full `JobTimeout` measured from the moment execution starts, so time spent queued no longer shortens the execution budget. The requesting replica sets `StartBy` to enqueue time plus `JobTimeout`, and keeps its in-process flight until `StartBy` plus `JobTimeout`. The distributed job lock lives one minute longer.

The job contract version is part of the pipeline profile hash (it is 5 in this release, 4 in the v4 release), so replicas running a different contract never exchange jobs (see Rolling deploy in Part 1).

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

Pipeline profiles include the cache schema, job contract version, output timeline offset and active accelerator. Replicas on different releases therefore subscribe to different queues and never exchange jobs, but a requester of the old release cannot be served by workers of the new one. The rollout steps are in "Rolling deploy" in Part 1.

Mixed-hardware replicas (for example CUDA and software) also use separate profiles and queues; this is intentional.

### Path generation and subtitles

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

### Controller construction

Use the error-returning constructor in applications:

```go
controller, err := goshl.NewControllerWithContext(ctx, options)
if err != nil {
    return err
}
```

`NewController` remains available as a panic-on-error compatibility wrapper.

Invalid durations, pool sizes, batch sizes, preview settings, thumbnail settings, stream types, renditions, and segment indexes now fail explicitly instead of reaching FFmpeg or waiting for a timeout.

### Hardware validation and CUDA modes

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

### Direct stream rules in v4

Direct H.264 remuxing is limited to inputs that are safe for browser HLS (Part 1 lists the further restrictions added in this release):

- Baseline, Main, or High profile.
- Known supported H.264 level.
- 8-bit `yuv420p` or `yuvj420p`.
- A random-access frame at the beginning.
- No unresolved rotation or anamorphic sample aspect ratio.

Other H.264 inputs transcode to deterministic High-profile `yuv420p` output. This may increase transcoding load for media that older Goshl versions copied directly.

Master playlists now distinguish audio codec and channel groups, include aggregate bandwidth and codec declarations, support silent and audio-only media, and expose subtitle groups.

### Operational behavior

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

### Sprites and thumbnails

- The sprite cache identity comes from the generator's own sprite settings (`160x90:5:10x10` by default). If the sprite settings ever change, the identity changes with them and sheets regenerate once, leaving the old ones orphaned in `SpriteStorage` until `Invalidate` or your own cleanup removes them. (Under the new cache schema all sprites regenerate once anyway.)
- The sprite VTT is no longer stored. `SpriteVTT` ensures the sprite sheets exist and renders the VTT on every request using the current `PathGenerator.Sprite`, so changing sprite routes (for example signed URLs) takes effect immediately. VTTs stored by earlier versions are ignored and can be deleted.
- `Invalidate` also purges `Options.SpriteStorage` when configured (see Cache migration).
- A zero `ThumbnailConfig.Position` means "use the default position" in both `Options.DefaultThumbnailConfig` and per-request configs, consistent with `Width`. A request with only `Width` set no longer thumbnails at position 0. Position 0.0 (the very first frame) is therefore not selectable; use a small positive value such as 0.001.

### Lifecycle and observability

- `Controller.Stop` is now final. After it, every method except `Stop`, `HWAccelConfig` and the constructors returns the new `ErrControllerStopped`. `Stop` cancels detached work (asset generation, metadata probes, flight watchers), waits up to 10 seconds, and releases the locks of flights that were never enqueued. Locks of already-enqueued jobs expire by TTL. Build a new controller instead of restarting.
- Metadata probe failures are cached in process for 10 seconds (up to 1024 sources); `Invalidate` for a source forgets its entry. A probe that exceeds `ProbeTimeout`, on this replica or the one holding the lock, surfaces as `ErrMetadataTimeout` (which also wraps `context.DeadlineExceeded`).
- New events: `EventPoolSubscriptionLost` (a worker pool's coordinator subscription ended) and `EventJobDeliveryRejected` (a malformed or wrong-profile delivery was refused, including when acknowledging it also failed). A delivery whose `StartBy` passed is settled failed with a start-by error and emits `EventJobFailed`.
- `EventLockWait` is also emitted for metadata and generated-asset locks.

### Removed API

- `Options.AbandonGrace` was never honored and has been removed.
- `EventJobAbandoned` and `EventSegmentFailed` were never emitted and have been removed.
- `WriteSpriteVTT`, `ReadSpriteVTT` and `SpriteVTTExists` are removed from `Storage` and `SpriteStorage`. Implementations may keep the methods; nothing calls them.
- `Job.Deadline` is replaced by `Job.StartBy` (new semantics, see above).
- `Coordinator.Close` and unused route methods are no longer part of the required interfaces.
- `SegmentData.SourceURL` is replaced by `SegmentData.MediaKey`.

### Checklist for the v4 changes

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
