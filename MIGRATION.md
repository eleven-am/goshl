# Migration

Upgrade one section at a time. Each section lists what you must do.

| From | To | Cache schema | Job contract | Effort |
|---|---|---|---|---|
| v0.0.25 | v0.0.26 | v3 to v5 | 5 | Coordinator change, cold cache, ffmpeg 6.1 |
| v0.0.26 | v0.0.27 | v5 to v6 | 5 | Cold cache, HDR rule change |
| v0.0.27 | v0.0.28 | v6 | 5 | None |

## v0.0.25 to v0.0.26

### Coordinator

- [ ] Carry every field of `Job`, `VideoRendition`, `AudioRendition` and `ColourPlan` unchanged. Fields you do not interpret still round-trip. A JSON or protobuf coordinator written for v0.0.25 drops the new ones. The fields new in v0.0.26 include `VideoRendition.AverageBandwidth`, `AudioRendition.AverageBandwidth`, `StripParameterSets`, `StripSEI` and `ColourPlan.Retag`. Add them before you deploy goshl. Extra fields are harmless to the old release.

| Struct | Fields to add or keep |
|---|---|
| `Job` | `StartBy`, `Trailing`, `VideoStreamIndex`, `Digest`, plus `ID`, `RunID`, `MediaKey`, `Profile`, `SourceURL`, `Rendition`, `StreamType`, `StartIndex`, `EndIndex`, `RequestedIndex`, `EnqueuedAt`, `Segments`, `Overlap`, `ExpectedIndices`, `VideoRendition`, `AudioRendition` |
| `VideoRendition` | `BufferSize`, `PeakBandwidth`, `AverageBandwidth`, `Colour`, `StripParameterSets`, `StripSEI`, `SourceEnd`, plus `Name`, `Width`, `Height`, `Bitrate`, `PeakBitrate`, `FrameRate`, `Codec`, `Level`, `Method` |
| `AudioRendition` | `SampleRate`, `PeakBandwidth`, `AverageBandwidth`, `SourceEnd`, `Title`, plus `Name`, `Codec`, `CodecString`, `Bitrate`, `Channels`, `Method`, `StreamIndex`, `Language`, `Default` |
| `ColourPlan` | `Tonemap`, `Retag`, `AssumeMatrix`, `AssumePrimaries` |

- [ ] Do not round `Job.Segments` times. Adjacency is compared with a tolerance of 1e-6 s.
- [ ] Store `EnqueuedAt` and `StartBy` at any precision. The digest excludes them.
- [ ] Rename `Job.Deadline` to `StartBy`. Its meaning changed: `StartBy` is the latest moment a worker may begin the job. A worker runs the job with a full `JobTimeout` from the start of execution.
- [ ] Remove the `Metadata` copy from `Job`. It no longer exists.
- [ ] Remove `AudioRendition.GroupID` from your schema.
- [ ] Remove `Coordinator.Close` from the interface you must satisfy. You own the coordinator lifecycle.
- [ ] Route `Subscribe(ctx, streamType, profile)` jobs only to subscribers whose stream type and profile both equal the job's.
- [ ] Wrap `goshl.ErrEnqueueRejected` in every `Enqueue` error where the job was definitely not persisted. Without it, a failed `Enqueue` blocks that batch until its lock TTL (`StartBy` + `JobTimeout` + 1 minute) ends.
- [ ] Make `Enqueue` idempotent by `RunID`.
- [ ] Give each delivery a unique receipt and a lease of at least 5 seconds. `Renew` extends that receipt. Redelivery invalidates the old receipt.
- [ ] Implement `SettleJob` as one atomic step, idempotent for the same receipt and status. Keep the receipt-to-status record and job status until one minute after `StartBy` plus `JobTimeout`.
- [ ] Limit `Ack` to malformed deliveries.
- [ ] Key `WaitJob` by `RunID`. Fan out to all waiters. Replay the latest status to late waiters. Terminal states are immutable.
- [ ] Make `AcquireJob` take the lock and register the run ID in one step. Return `ErrLockHeld` and the active run ID when the lock is held. Make `ReleaseJob` check job ID, run ID and token.
- [ ] Do not replay `NotifySegment`. Unregister `WaitJob` and `WaitSegment` subscribers when their context ends.

### Rollout

- [ ] Deploy the coordinator change first.
- [ ] Drain queued jobs of the old release. v0.0.26 workers use a different profile and never take them.
- [ ] Upgrade producers and workers together.
- [ ] Watch for `EventJobDeliveryRejected` with a `digest mismatch` error. It names a coordinator that changes jobs.

### Cache

- [ ] Expect a cold cache. Cache schema `goshl-cache-v5` replaces v3. Every source is re-probed and re-transcoded once. Every subtitle, sprite, preview and thumbnail is generated again.
- [ ] Delete cold v3 data when convenient.
- [ ] Store media and asset keys exactly as `Storage` receives them. Never derive keys from the source URL. Never open a key as a URL or file.
- [ ] For remote sources, return a stable ETag, version or fingerprint from `Options.SourceRevision`. Without it the full URL is the revision, and a refreshed signed URL starts a new namespace.
- [ ] To use `Controller.Invalidate`, implement `DeleteMedia(ctx, mediaKey) error` on `Storage` and on any `SpriteStorage`.

### ffmpeg and sources

- [ ] Install `ffmpeg` and `ffprobe` 6.1 or newer on every replica. The constructor returns an error wrapping `ErrInvalidOptions` otherwise.
- [ ] For HDR media, use an ffmpeg with `zscale` and `tonemap`, the same build on every replica. Handle `ErrTonemapUnavailable` from `MasterPlaylist` and the video `VariantPlaylist`.
- [ ] Pass only local paths, `file:`, `http:` or `https:` sources. Everything else returns `ErrUnsupportedSource`.
- [ ] Validate user-supplied source URLs in your application. goshl does not restrict hosts. See [docs/operations.md](docs/operations.md#trust-model-and-source-urls).
- [ ] Do not pass HLS or ffconcat playlists as sources. They are rejected.
- [ ] Serve remote MP4 and MKV sources from hosts that support single byte ranges (`206 Partial Content` with an accurate `Content-Range`). Others use the slower ffmpeg path.

### API

- [ ] Use `NewControllerWithContext` in applications. `NewController` still exists and panics on error.
- [ ] Update `PathGenerator.SubtitleVTT` and `SubtitlePlaylist` handlers to take a track ID such as `subtitle-3`, not a language.
- [ ] Serve both `SubtitlePlaylist` and `SubtitleVTT`.
- [ ] Remove uses of the `DirectPlay` constant. It never fired.
- [ ] Handle subtitle errors: unconvertible and unknown tracks return `ErrRenditionNotFound`. The text for an unknown ID is `rendition not found: subtitle track X`.
- [ ] Keep `Options` inside bounds: `TargetDuration` 1 to 60 s, `SegmentsPerJob` up to 1000, pool sizes up to 1024.
- [ ] Optional: subscribe to `EventRenditionDropped`.
- [ ] Optional: read the new fields `Metadata.Bitrate`, `VideoStream.End` and `AudioStream.End`.

### Behaviour changes

- Output media starts at 10 seconds on its timeline. Subtitle WebVTT begins with `X-TIMESTAMP-MAP=MPEGTS:900000,LOCAL:00:00:00.000`. Keep that line if you post-process subtitles.
- Segment counts and indices change for most sources. The planner caps a segment at `max(2 x TargetDuration, 12 s)`.
- Software video uses libx264 `superfast` instead of `ultrafast`. Output is High profile. Expect about 1.5 times the CPU per software video job.
- Sources with a keyframe gap above the cap lose direct stream and transcode their source-resolution rung.
- A video that ends more than 0.5 s before the file repeats its last frame. No copied rung is offered for it.
- Copied H.264 loses SEI units unless the source carries closed captions, and loses trailing parameter-set-only access units.
- All transcodes are tagged BT.709 limited range. HDR sources are tone mapped. HDR is never direct-streamed.
- `VideoStream.FrameRate` is the average frame rate. A 50 field-per-second source reports 25.
- `Duration` is bounded by known stream ends. Truncated sources report the last readable packet.
- MP4 header I/O errors fail the probe instead of falling back to a packet scan.
- MKV and WebM keyframes come from Matroska Cues when no gap exceeds 11.5 s. Otherwise the probe scans.
- Master playlists follow RFC 8216 audio groups: `NAME` is the track title or language label, one `DEFAULT` per group, `LANGUAGE` is an RFC 5646 tag.
- Subtitle `DEFAULT` follows the source disposition. Names are unique. `FORCED=YES` implies `AUTOSELECT=YES`.
- Playlist methods return errors instead of invalid playlists.
- Only text subtitle codecs are listed and served. Bitmap tracks and unlisted codecs such as TTML are not.
- Auto-detect validates CUDA, QSV, VideoToolbox and VAAPI in order. `HWAccelConfig().Error` can be set while hardware is selected. The constructor worst case with `HWAccel: true` is 70 s.
- CUDA defaults to `DecodeSafe`. `DecodeDevice` opts into the device-frame graph.
- VideoToolbox no longer emits a keyframe every 12 frames (`-g 250`).
- Audio from 44.1 kHz and 32 kHz sources joins frame-exactly across jobs.
- Every job output is validated against the planned segment times. A mismatch fails the job.
- Each TS segment sets the MPEG-TS discontinuity indicator on its first packet.
- `Start` returns `ErrControllerStopped` after `Stop`. Lock-held waiters return `ErrControllerStopped` on `Stop`.
- A failed single-track subtitle batch is not rerun with the same command.

### Verify

- [ ] Play and seek across segment boundaries in Safari.
- [ ] Play sources whose video starts slightly after the container start.
- [ ] Run a playback and seek test in each target GPU environment.

## v0.0.26 to v0.0.27

- [ ] Roll every replica together. Cache schema `goshl-cache-v6` replaces v5, and v5 and v6 replicas never exchange jobs. The job contract stays 5. Coordinators need no change.
- [ ] Expect a cold cache. Every source is re-probed and re-transcoded once and every generated asset is produced again. Delete cold v5 data when convenient.
- [ ] Install an ffmpeg with `zscale` and `tonemap` wherever 8-bit PQ or HLG sources with BT.2020 or unspecified primaries are served. They are now tone mapped, where v0.0.26 retagged them as BT.709. Without the filters their `MasterPlaylist` and video `VariantPlaylist` return an error wrapping `ErrTonemapUnavailable`.

The HDR rule, at any bit depth:

| Transfer | Primaries | Treatment |
|---|---|---|
| PQ or HLG | `bt2020` or unspecified (assumed BT.2020) | Tone mapped |
| PQ or HLG | `bt709`, `smpte170m`, `bt470bg`, `bt470m` | Mis-tagged SDR. Retagged BT.709 without tone mapping. v0.0.26 tone mapped a 10-bit source tagged this way. |

HDR sources are never direct-streamed.

## v0.0.27 to v0.0.28

Nothing to do. The release restructures internals. The public API, cache schema and job contract are unchanged.
