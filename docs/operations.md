# Operations

## ffmpeg

| Requirement | Detail |
|---|---|
| Version | `ffmpeg` and `ffprobe` 6.1 or newer in `PATH`. |
| Startup check | The constructor runs both binaries. An older or unrecognised version returns an error wrapping `ErrInvalidOptions`. Development builds (`git-...`, `N-...`) pass. |
| HDR filters | `zscale` (libzimg) and `tonemap`, for example Homebrew `ffmpeg-full` or `jellyfin-ffmpeg`. Detected on first use and cached. |
| Fleet | The same build on every replica. The HDR check runs on the host that serves the playlist. |

### ffmpeg base image

`docker/ffmpeg` builds a runtime base image with ffmpeg and ffprobe. Tags are immutable. Pin the digest.

| Tag | ffmpeg | Platforms | Digest |
|---|---|---|---|
| `ghcr.io/eleven-am/goshl-ffmpeg:9.0.2-1` | 9.0.2 | linux/amd64, linux/arm64 | `sha256:49618bfc33fd0a8e9705be5aae97129172b596bc3a1b69a9fd03664a2ccfcaef` |

```dockerfile
FROM ghcr.io/eleven-am/goshl-ffmpeg:9.0.2-1@sha256:49618bfc33fd0a8e9705be5aae97129172b596bc3a1b69a9fd03664a2ccfcaef
```

Contents:

- Shared-library build from the official ffmpeg source on `ubuntu:24.04`, against nv-codec-headers 13.0.19.0 (NVENC API 13.0). NVENC needs NVIDIA driver 570 or newer.
- `libx264`, `h264_nvenc`, `h264_vaapi`, `h264_qsv` (amd64), `aac`, `ac3`, `eac3`, `png`, `mjpeg` and the subtitle codecs.
- Filters `scale_cuda`, `zscale`, `tonemap`, `tonemap_vaapi` and `tonemap_opencl`.
- `gosu`, `passwd`, `ca-certificates`, `tzdata`, `wget` and `NVIDIA_DRIVER_CAPABILITIES=compute,utility,video`.

`docker/ffmpeg/selfcheck.sh` runs as the last build step. The build fails unless every required encoder, filter, muxer and library is present, NVENC targets API 13.0, and software, HLS, HDR tone-mapping and subtitle smoke encodes succeed. Hardware encoders are checked for presence only.

Release a new image:

```bash
git tag ffmpeg-9.0.2-2
git push origin ffmpeg-9.0.2-2
```

The tag triggers `.github/workflows/ffmpeg-image.yml`. It builds each platform on a native runner and refuses to overwrite an existing tag. The workflow also runs on manual dispatch with a `version` input.

Build locally:

```bash
docker buildx build --platform linux/amd64,linux/arm64 -t <image>:<tag> --push docker/ffmpeg
```

## Hardware acceleration

```go
Options{HWAccel: true}
Options{HWAccelMode: goshl.AccelCUDA, HWAccelDecodeMode: goshl.DecodeSafe}
```

### Auto-detect

`HWAccel: true` detects accelerators for up to 10 seconds. It then validates each detected accelerator with a decode, filter and encode self-test, in this order, 15 seconds per accelerator:

1. CUDA
2. QSV
3. VideoToolbox
4. VAAPI

The first accelerator that passes is selected. If none passes, goshl uses software. The worst case is 70 seconds. Pass a context with a deadline to `NewControllerWithContext` to cap it.

### Explicit mode

`HWAccelMode` selects one accelerator and overrides `HWAccel`. Goshl validates it once with a 15-second bound. When validation fails, construction returns an error. With `AllowHardwareFallback: true` it selects software instead and records the failure.

### Reporting

`HWAccelConfig().Error` and `EventHardwareFallback` list the failure of every skipped accelerator, including when a lower-priority accelerator was selected. `EventHardwareSelected` reports the final choice.

### Decode modes

| Mode | Behaviour |
|---|---|
| `DecodeSafe` | Default. NVDEC decodes without retaining frames in the CUDA surface pool. Scaling runs on host frames before NVENC. |
| `DecodeDevice` | Full device-frame decode, scale and encode graph. |

### Encoders

Software encoding uses libx264 `superfast`, which produces the High profile the playlists advertise. VideoToolbox encodes with `-g 250`, so only the forced segment keyframes appear.

### Pipeline profiles

The accelerator and its flags are part of the pipeline profile. Workers on different accelerators never exchange jobs. A mixed fleet runs separate queues.

## HDR

A source is HDR when its transfer is PQ (`smpte2084`) or HLG (`arib-std-b67`). Colour primaries decide the treatment. Bit depth does not.

| Transfer | Primaries | Treatment | Needs `zscale` and `tonemap` |
|---|---|---|---|
| PQ or HLG | `bt2020` | Tone mapped to SDR | yes |
| PQ or HLG | unspecified | Assumed BT.2020, tone mapped | yes |
| PQ or HLG | `bt709`, `smpte170m`, `bt470bg`, `bt470m` | Mis-tagged SDR. Retagged BT.709, encoded normally | no |
| other | any | SDR path | no |

Every transcode is tagged BT.709 limited range. Untagged SDR sources are assumed BT.709 (1280 pixels wide or more, or over 576 lines) or SMPTE 170M. Full-range sources are converted instead of emitted as `yuvj420p`. HDR sources are never direct-streamed.

### ErrTonemapUnavailable

When a source needs tone mapping and ffmpeg lacks `zscale` or `tonemap`, `MasterPlaylist` and the video `VariantPlaylist` return an error wrapping `ErrTonemapUnavailable`.

```go
playlist, err := controller.MasterPlaylist(ctx, src)
if errors.Is(err, goshl.ErrTonemapUnavailable) {
	http.Error(w, "HDR playback unavailable on this host", http.StatusServiceUnavailable)
	return
}
```

SDR sources are unaffected.

## Cache identity and schema

The cache schema is `goshl-cache-v6`.

```
mediaKey   = hash(schema, sourceURL, revision, pipelineVersion)
assetKey   = mediaKey + ":asset:" + hash(kind, config)
```

| Part | Source |
|---|---|
| `schema` | `goshl-cache-v6` |
| `sourceURL` | The URL as passed to the controller. |
| `revision` | `SourceRevision`; for a local file its size and modification time; otherwise the URL. |
| `pipelineVersion` | Hash of schema, `Options.CacheVersion`, `TargetDuration`, accelerator, decode and encode flags, scale filter, decode mode, timeline offset and job contract version. |

`Controller.MediaKey` returns the media key. Treat keys as opaque. Do not open a key as a URL or file. Store them exactly as given. Asset keys sit under the media key so `DeleteMedia` removes metadata, segments, sprites, subtitles, previews and thumbnails together.

The pipeline profile sent in `Job.Profile` is the `pipelineVersion`. Replicas with different profiles never exchange jobs.

### Invalidation

| Action | Effect |
|---|---|
| `Controller.Invalidate(ctx, src)` | Calls `DeleteMedia` on `Storage` and on `SpriteStorage`. Needs `CacheInvalidator` on each, otherwise returns `ErrCacheInvalidationUnsupported` before deleting. Also forgets a remembered probe failure. |
| Change `Options.CacheVersion` | Starts a new cache namespace for every source. |
| Change `SourceRevision` result | Starts a new namespace for that source. |
| Change `TargetDuration`, accelerator or decode mode | Starts a new namespace for every source. |
| Upgrade goshl to a new schema or job contract | Starts a new namespace. Old objects stay in storage as cold data. Delete them when convenient. |

## Coordinator contract

### Job fields

A coordinator carries every field of `Job`, `VideoRendition`, `AudioRendition` and `ColourPlan` unchanged, including fields it does not interpret. The job contract version is 5.

| `Job` field | Meaning |
|---|---|
| `ID` | Deterministic batch identity. Locking key. |
| `RunID` | One execution of the batch. Queue idempotency key. |
| `MediaKey` | Opaque media identity. |
| `Profile` | Pipeline profile. Routing key with `StreamType`. |
| `SourceURL` | The source ffmpeg reads. |
| `Rendition`, `StreamType` | The rendition and stream the job produces. |
| `StartIndex`, `EndIndex`, `RequestedIndex`, `ExpectedIndices` | The segments the job produces. |
| `EnqueuedAt` | Enqueue time. |
| `StartBy` | Latest moment a worker may begin the job. |
| `Segments` | Exact boundaries as `float64` seconds. Do not round them. Adjacency is checked with a tolerance of 1e-6 s. |
| `Overlap` | Segment before the first one. Set for audio jobs and copied-video jobs that do not start at segment 0. |
| `Trailing` | One segment after the last one for copied video and audio jobs. Produced and discarded. |
| `VideoRendition` | Pointer, set for video jobs. |
| `AudioRendition` | Pointer, set for audio jobs. |
| `VideoStreamIndex` | Source video stream the worker encodes. |
| `Digest` | SHA-256 hex over every other field except `EnqueuedAt` and `StartBy`. |

| `VideoRendition` field | Meaning |
|---|---|
| `Name`, `Width`, `Height`, `Codec`, `Level`, `FrameRate` | Rendition identity and shape. |
| `Bitrate`, `PeakBitrate` | Encoder rate control. |
| `BufferSize` | Rate-control buffer, derived from the H.264 level. |
| `PeakBandwidth`, `AverageBandwidth` | Values for `BANDWIDTH` and `AVERAGE-BANDWIDTH`. |
| `Method` | `direct_stream` or `transcode`. |
| `Colour` | `ColourPlan`: `Tonemap`, `Retag`, `AssumeMatrix`, `AssumePrimaries`. |
| `StripParameterSets` | Remove trailing parameter-set-only access units from copied H.264. |
| `StripSEI` | Remove SEI units from copied H.264. |
| `SourceEnd` | Video end in seconds when it ends more than 0.5 s before the file, otherwise 0. |

| `AudioRendition` field | Meaning |
|---|---|
| `Name`, `Codec`, `CodecString`, `Bitrate`, `Channels`, `Method` | Rendition identity and encoding. |
| `SampleRate` | Source sample rate, used for passthrough audio. |
| `PeakBandwidth`, `AverageBandwidth` | Values for `BANDWIDTH` and `AVERAGE-BANDWIDTH`. |
| `SourceEnd` | End of the source audio in seconds, 0 if unknown. |
| `StreamIndex` | Source audio stream. |
| `Language`, `Title`, `Default` | Track labels. |

### Digest

The digest covers the whole `Job`, including every nested rendition and `ColourPlan` field. A coordinator that drops any field gets the job rejected. The producer sets `Digest`. A worker recomputes it and refuses the delivery when it differs. The job settles failed with a `digest mismatch` error and the worker emits `EventJobDeliveryRejected`. `EnqueuedAt` and `StartBy` are excluded, so a coordinator may store them at any precision or in any zone.

A worker also refuses a job with an unknown `Method`, a transcoded audio rendition without `Channels` or `Bitrate`, a passthrough audio rendition without `Codec`, an audio or copied-video job after segment 0 without `Overlap`, and an `audioN_...` rendition whose `StreamIndex` is not N.

### Behaviour

- A delivered `Job` equals the enqueued one field for field.
- `Enqueue` is idempotent by `RunID`. Wrap `ErrEnqueueRejected` only when the job was definitely not persisted. Goshl then frees the batch so the next request retries. Any other error is ambiguous and the batch stays reserved until its lock expires.
- `Subscribe` is a competing-consumer subscription scoped by stream type and profile. A job goes only to a subscriber with its exact `Job.Profile`. Each delivery has a unique receipt and a lease of at least five seconds.
- `Renew` extends one delivery. An unacknowledged delivery returns to the queue when its lease expires. Redelivery invalidates the previous receipt.
- `SettleJob` validates the active receipt, persists the terminal status and removes the delivery in one step. It is idempotent for the same receipt and status. Keep the receipt-to-status record and the job status until at least one minute after `StartBy` plus `JobTimeout`.
- `Ack` is only for malformed deliveries that cannot supply a valid status.
- `WaitJob` is keyed by `RunID`, fans out to all waiters and replays the latest status to a late waiter. Terminal states are immutable.
- `AcquireJob` takes the batch lock and registers the run ID in one step. When the lock is held it returns `ErrLockHeld` and the active run ID. `ReleaseJob` clears the lock only when job ID, run ID and token all match.
- `NotifySegment` and `WaitSegment` are a transient hint. They do not replay. Failures travel through job status.
- `WaitJob` and `WaitSegment` unregister their subscribers when the context ends.
- Goshl never cancels a cluster job because one replica has no local waiters.

### Timing

A worker refuses a delivery whose `StartBy` has passed and settles it failed. Otherwise it runs the job with a full `JobTimeout` from the start of execution. The batch lock lives `StartBy` + `JobTimeout` + 1 minute.

## Trust model and source URLs

Allowed sources: local paths, `file:`, `http:` and `https:` URLs. Any other scheme, an empty source or a malformed URL returns an error wrapping `ErrUnsupportedSource`.

| Rule | Detail |
|---|---|
| Protocol whitelist | Every ffmpeg and ffprobe call runs with `file,http,https,tcp,tls`. `tcp` and `tls` exist for ffmpeg's own use under `http(s)`. `rtmp://`, `ftp://`, `concat:` and `data:` are rejected before ffmpeg starts. |
| Format whitelist | Inputs are limited to container formats (`-format_whitelist`). HLS and ffconcat playlists are rejected, so a source cannot make ffmpeg open further URLs or files. |
| Host policy | goshl treats `sourceURL` as trusted. It does not restrict hosts, including loopback and private addresses. |
| Redirects | goshl's own range reader follows at most 5 redirects, only to `http(s)`, and refuses an https-to-http downgrade. ffmpeg and ffprobe follow redirects under their own limits. |
| TLS | Certificate verification by ffmpeg depends on its TLS backend. goshl does not enforce it. |
| Error output | ffmpeg and ffprobe stderr are bounded. Source URLs, credentials, query strings and media paths are redacted from returned command errors. |

If users can supply source URLs, validate them in your application and limit outbound network access to the origins you intend.

## Output timeline

Generated video and audio segments start at 10 seconds on the output timeline, not 0. Every rendition shares one continuous clock across segment boundaries. Players need no change.

Subtitle WebVTT files begin with:

```
X-TIMESTAMP-MAP=MPEGTS:900000,LOCAL:00:00:00.000
```

Keep this line if you post-process stored or served subtitle files. Sprite, preview and thumbnail output is not shifted. The offset is part of the pipeline profile.

Each TS segment sets the MPEG-TS discontinuity indicator on its first packet.

## Observability

Set `Options.Observer` to receive an `Event` for each item in the table. `ObserverFunc` adapts a function. Return from callbacks promptly. Event identities are opaque hashes. Raw signed source URLs are not emitted.

```go
Options{Observer: goshl.ObserverFunc(func(ctx context.Context, e goshl.Event) {
	switch e.Type {
	case goshl.EventJobFailed, goshl.EventJobDeliveryRejected:
		log.Printf("%s job=%s err=%s", e.Type, e.JobID, e.Error)
	}
})}
```

| Group | Events | Watch for |
|---|---|---|
| Metadata | `EventMetadataCacheHit`, `EventMetadataCacheMiss`, `EventMetadataProbed` | Miss with `Error` set means unreadable stored metadata. It is probed again. |
| Segments | `EventSegmentCacheHit`, `EventSegmentCacheMiss`, `EventSegmentReady`, `EventSegmentNotifyFailed` | Cache hit ratio, request latency in `Duration`. |
| Coordination | `EventLockWait`, `EventJobEnqueued`, `EventJobJoined`, `EventLockReleaseFailed` | Waits on another replica's lock (segment batch, metadata probe or generated asset). |
| Workers | `EventJobStarted`, `EventJobCompleted`, `EventJobFailed`, `EventPoolSubscriptionLost` | Failures and lost subscriptions. |
| Delivery | `EventJobAckFailed`, `EventJobLeaseFailed`, `EventJobSettleFailed`, `EventJobDeliveryRejected` | Coordinator faults. A `digest mismatch` rejection names a coordinator that alters jobs. A job past its `StartBy` is `EventJobFailed`, not a rejection. |
| Hardware | `EventHardwareSelected`, `EventHardwareFallback` | Skipped accelerators and why. |
| Ladder | `EventRenditionDropped` | A video rung needing an H.264 level above 5.2. `Rendition` and `Error` carry the name and reason. |
