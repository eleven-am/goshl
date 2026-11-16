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
| `pipelineVersion` | Hash of schema, `Options.CacheVersion`, `TargetDuration`, accelerator, decode and encode flags, scale filter, decode mode, timeline offset and output contract version. |

`Controller.MediaKey` returns the media key. Treat keys as opaque. Do not open a key as a URL or file. Store them exactly as given. Asset keys sit under the media key so `DeleteMedia` removes metadata, segments, sprites, subtitles, previews and thumbnails together.

`pipelineVersion` hashes the output contract version (5), not the job contract version. The profile sent in `Job.Profile` is the route profile: a hash of `pipelineVersion` and the job contract version (6). Replicas with different route profiles never exchange jobs. A job contract change therefore re-routes work without emptying the cache.

### Invalidation

| Action | Effect |
|---|---|
| `Controller.Invalidate(ctx, src)` | Calls `DeleteMedia` on `Storage` and on `SpriteStorage`. Needs `CacheInvalidator` on each, otherwise returns `ErrCacheInvalidationUnsupported` before deleting. Also forgets a remembered probe failure. |
| Change `Options.CacheVersion` | Starts a new cache namespace for every source. |
| Change `SourceRevision` result | Starts a new namespace for that source. |
| Change `TargetDuration`, accelerator or decode mode | Starts a new namespace for every source. |
| Upgrade goshl to a new cache schema or output contract | Starts a new namespace. Old objects stay in storage as cold data. Delete them when convenient. |

## Coordinator contract

### Job fields

A coordinator must round-trip every `Job` field, because the digest covers the whole job. It carries every field of `Job`, `VideoRendition`, `AudioRendition`, `ColourPlan` and `ChannelSpec` unchanged, including fields it does not interpret. The job contract version is 6.

| `Job` field | Meaning |
|---|---|
| `ID` | Deterministic batch identity. Locking key. |
| `RunID` | One execution of the batch. Queue idempotency key. |
| `MediaKey` | Opaque media identity. |
| `Profile` | Route profile. Routing key with `StreamType`. |
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
| `Priority` | `JobPriority`. Delivery order inside one route. 100 for a `Segment` miss, 200 for a channel job. |
| `Channel` | `*ChannelSpec`. Set on channel jobs only. Carries the format version, canvas, picture, frame rate, frames per segment, batch size, total frames, audio track, silence flag, phase and `SourceFrameRate`. |
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

A worker refuses a channel job whose renditions are not named like the job, whose range is not grid-aligned, or whose `ChannelSpec` format version is unknown. It refuses a non-channel job that carries a `ChannelSpec`. A worker also refuses a job with an unknown `Method`, a transcoded audio rendition without `Channels` or `Bitrate`, a passthrough audio rendition without `Codec`, an audio or copied-video job after segment 0 without `Overlap`, and an `audioN_...` rendition whose `StreamIndex` is not N.

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
- Deliver jobs in this order inside one stream type and profile: highest `Priority` first, oldest first within a priority, and a redelivered job ahead of its own priority. Do not buffer the `Subscribe` channel, or a late high-priority job waits behind buffered ones.
- Route `StreamChannel` jobs to subscribers of `StreamChannel`. Treat the stream type as an opaque routing key.

### Timing

A worker refuses a delivery whose `StartBy` has passed and settles it failed. Otherwise it runs the job with a full `JobTimeout` from the start of execution. The batch lock lives `StartBy` + `JobTimeout` + 1 minute.

## Live

`Controller.Live` serves a raw MPEG-TS byte stream as sliding-window HLS. It is for IPTV providers that expose a plain TS URL.

### Reader input

The host supplies `LiveSource.Open`, which returns an `io.ReadCloser`. goshl never sees the URL, the credentials or the network. The host keeps its own HTTP client, authentication, proxies and provider failover. ffmpeg reads from a pipe and cannot open a file or URL.

- `Close` must unblock a pending `Read`. `http.Response.Body` does.
- goshl calls `Open` with a context that lives as long as it uses the reader, and calls `Close` when done.
- A reader error, EOF or 10 seconds without bytes ends the reader. goshl reopens the source: immediately, then after 0.5, 1, 2 and 4 seconds. Five attempts. The counter resets once a reader produced a segment. The first `Open` is not retried.
- A timestamp jump inside one reader does not break the output. ffmpeg re-bases it.
- Output timestamps keep rising across reconnects and `SwitchSource`. Each new reader starts with `EXT-X-DISCONTINUITY`.

### Codec rules

| Input | Output |
|---|---|
| H.264, `yuv420p` or `yuvj420p` | Copied. Interlaced H.264 is copied untouched. |
| H.264 in another pixel format (4:2:2, 10-bit) | Transcoded to H.264. |
| HEVC or MPEG-2 | Transcoded to H.264, progressive, at most `MaxHeight`, tagged BT.709. Interlaced input is deinterlaced with `bwdif`. |
| Anything else, or no video | `ErrLiveUnsupportedCodec`. |
| AAC | Copied, any channel count and sample rate. |
| AAC-LATM, MP2, MP3, AC-3, E-AC-3 | AAC stereo, 48 kHz, 128 kbit/s. |
| Other audio | Skipped. With no usable audio the output is video only. |

The first decodable audio track is used, or the first whose language equals `PreferredLanguage`. There are no subtitles and no data streams. Audio-only sources are refused. Transcoded video uses the VOD encoder, tone mapping and hardware choice. The bitrate follows the ladder class of the output short side: 800 kbit/s up to 360, 1.2 Mbit/s up to 480, 2.5 Mbit/s up to 720, 5 Mbit/s up to 1080, and 15 Mbit/s above.

`LiveProbe` reports the same decision without starting a session.

### Slots and capacity

A copy session takes no encoder slot. A session that transcodes video takes one slot and one unit of `LiveMaxTranscodes` while it runs. It keeps the slot across reconnects and transcode-to-transcode switches. The slot is released when a switch makes the session copy, and when the session ends.

A live session never waits for a slot. With none free, `Live` still returns, and `WaitReady` and `State` report `ErrLiveCapacity`. `SwitchSource` returns `ErrLiveCapacity` and the old source keeps playing. See [configuration.md](configuration.md#encoder-slots).

Software transcoding must run faster than real time. If it does not, goshl stops reading, the provider drops the connection, and the session loops through reconnects. Size `LiveMaxTranscodes` to the CPU or encoder.

### Idle stop

Every `WaitReady`, `Playlist`, `Segment`, successful `SwitchSource` and repeated `Live` call resets the idle timer. A failed `SwitchSource` does not. After `IdleTimeout` without a touch the session stops: ffmpeg is killed, the reader is closed and the files are deleted. A later `Live` for the key starts fresh. `Controller.Stop` stops every session.

A session keeps `WindowSegments` + 4 segments in the OS temp directory. At 8 Mbit/s and the defaults that is about 40 MB.

### Failure classification

| Situation | Result |
|---|---|
| Empty key, nil `Open`, options out of range | `ErrLiveInvalidOptions` from `Live` |
| `LiveMaxSessions` reached | `ErrLiveCapacity` from `Live` |
| First `Open` fails | Session fails with `ErrLiveSourceUnavailable`, wrapping the reader's error |
| No bytes in the probe budget, or data that is not MPEG-TS | `ErrLiveNoData` |
| Valid MPEG-TS that ends inside the probe | `ErrLiveSourceUnavailable` |
| Audio only, or an unsupported video codec | `ErrLiveUnsupportedCodec`, never retried |
| Transcode needed and no slot | `ErrLiveCapacity`, never retried |
| Read error, EOF or stall while running | Reconnect with `EventLiveReconnecting`. State stays `LiveRunning`. After five failed attempts the session fails with `ErrLiveSourceUnavailable`. |
| ffmpeg exits on its own | Same retry path. The final error has no live sentinel and its event class is `internal`. |
| `Stop`, idle stop, `Controller.Stop` | `ErrLiveStopped` from the methods. `State` is `LiveStopped` with a nil error. |

A suggested HTTP mapping: source unavailable and no data 502, unsupported codec 415, capacity 503, stopped and segment not found 404.

### Privacy

Live events and ffmpeg errors carry only `LiveSource.Label`. The session key, the reader's error text and the directory never appear in an event. Reader errors such as `dial tcp: lookup host` can name a provider host, so goshl never copies them into an event. The error returned to your code wraps the reader's error unchanged. Sanitise it before you log it. ffmpeg arguments never contain a URL. Use a label that identifies nothing secret, such as an internal channel ID.

## Channels

Channels serve linear TV from VOD sources. Every item uses one output format, so codec parameters never change at an item boundary and a player needs no reset. The host owns the schedule and playlist. goshl owns the format, the segment plan, segment production and prefetch.

### Format

| Property | Value |
|---|---|
| Video | H.264 High, 8-bit `yuv420p`, level 3.1, 2.5 Mbit/s capped, no B-frames |
| Frame rate | 24000/1001, constant |
| Picture | Fitted into 1280x720 with the aspect ratio kept, padded with black to exactly 1280x720 |
| Audio | AAC-LC stereo, 48 kHz, 160 kbit/s, muxed into the same TS segment |
| Segments | 48 frames, 2.002 s, one keyframe at the start. Segment `i` starts at `i` x 2.002 s |
| Colour | BT.709 limited range. HDR is tone mapped as in VOD |
| Timeline | Starts at 10 s for every item |
| Metadata | Source metadata stripped. Service provider `goshl`, service name `channel` |

Write `EXT-X-DISCONTINUITY` at every item change. `EXT-X-TARGETDURATION` 3 covers every item. The last segment of an item holds 12 to 59 frames at the defaults. A remainder under half a second merges into the previous segment.

A channel profile changes height, frame rate, segment size or bitrates. See [api.md](api.md#channels).

Channel jobs always encode on the CPU with libx264, whatever accelerator the controller uses, with libx264's default threading. They take no encoder slot. Output is not byte-reproducible: encoding the same segment twice gives different bytes. Correctness does not depend on it. Segments are served from storage, and joins between batches are frame-exact. A segment missing from a stored batch re-runs that whole batch, which rewrites its segments with new bytes and the same frames and timing. Run the same ffmpeg image on every replica so the format stays identical across replicas.

### Prefetch and lead

```go
for range time.Tick(time.Second) {
	next := playhead + 1
	if err := controller.ChannelPrefetch(ctx, src, profile, track, next, 15); err != nil {
		log.Printf("prefetch: %v", err)
	}
}
```

`ChannelPrefetch` enqueues work and returns. Call it on a timer with the segments after the playhead. Overlapping ranges cost nothing while a batch is in flight or stored: each batch is enqueued once. A lead of 15 segments (30 s) with a one-second tick was tested.

Batches sit on a fixed grid of `ChannelSegmentsPerJob` segments. A job always covers a whole batch, so concurrent requests and prefetches for any segment in it join the same job instead of encoding it twice. A cold `ChannelSegment` call can wait for up to a batch: 1.5 to 2 s on 1080p sources, 5.4 s on 4K PQ in the test setup. A smaller `ChannelSegmentsPerJob` shortens the wait and raises the overlap cost. Tune it with `ChannelPlan`, then prefetch before the item starts.

Readiness needs no new API. Check `Storage` with `plan.SegmentData(i)`, or watch `NotifySegment`.

### Lanes and priority

Channel jobs run in their own stream type (`StreamChannel`) and worker pool, sized by `ChannelPoolSize`. A saturated VOD video lane does not delay them. Priority orders jobs inside a lane and cannot preempt a running job.

| Priority | Value | Used by |
|---|---|---|
| `PriorityChannel` | 200 | Channel jobs |
| `PriorityPlayback` | 100 | `Segment` misses |
| `PriorityBackground` | 0 | Reserved |

Lanes isolate queue slots, not CPU. A channel encode still competes with VOD ffmpeg processes on the same cores.

### CPU cost

Measured with libx264 `superfast` at 3 Mbit/s to 1280x720, slightly above the 2.5 Mbit/s channels ship with, so the figures slightly overestimate the default. One job, on an 11-vCPU Docker VM shared with other work. The figures are noisy. Raw output is in [design/channels/cpu-speed-results.txt](design/channels/cpu-speed-results.txt).

| Source | Speed | Cores per content second |
|---|---|---|
| 1080p H.264 or HEVC | 9.8 to 12.7x | 0.5 to 0.7 |
| 1440p | 8.2 to 8.9x | 0.6 to 0.8 |
| 4K SDR | 6.0 to 10.6x | 0.8 to 1.4 |
| 4K 8-bit PQ, tone mapped | 4.6 to 5.3x | 1.1 to 1.3 |
| 4K 10-bit PQ HEVC, tone mapped | 3.7 to 4.1x | 1.6 to 1.9 |

Tone mapping at source resolution drops 4K PQ to about 1x, so goshl scales first. A steady channel needs well under one core. Plan CPU for the number of channels times the cold-batch bursts, and measure on your own hosts with `docs/design/channels/bench.sh` inside the goshl-ffmpeg image.

### Coordinator needs contract 6

Channel jobs carry `Job.Priority` and `Job.Channel`, and route on `StreamChannel`. A coordinator that drops either field makes workers reject the job with a `digest mismatch`. One that ignores priority still works but loses the ordering. See [Coordinator contract](#coordinator-contract).

## VOD metadata stripping

VOD video and audio segments no longer carry source metadata. Jobs run with `-map_metadata -1 -map_chapters -1`, so the source title, service name and language descriptors are gone. The TS service name becomes the ffmpeg default.

The media key, the cache schema and job IDs are unchanged. Segments already cached keep the source title until they are generated again. Old and new segments mix in one playlist without a player error. Call `Invalidate` for a source to regenerate it.

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
| Live | `EventLiveStarted`, `EventLiveReady`, `EventLiveReconnecting`, `EventLiveSwitched`, `EventLiveFailed`, `EventLiveStopped` | Reconnect loops and failures. Events carry `Label` and a class word in `Error`, never reader text. |
| Ladder | `EventRenditionDropped` | A video rung needing an H.264 level above 5.2. `Rendition` and `Error` carry the name and reason. |
