# Channels and live: design history

Status: channels implemented in v0.0.29. Live MPEG-TS sessions implemented in v0.0.29. Requested by frames (linear TV built from playlists).

The shipped behaviour is documented in [operations.md](../operations.md#channels) and [api.md](../api.md#channels). This page keeps the design record.

Supporting material is in [channels/](channels/):

| File | Contents |
|---|---|
| [api-design.md](channels/api-design.md) | API, engine integration points, lanes and priority, cache identity, test plan, open questions for frames |
| [output-format-report.txt](channels/output-format-report.txt) | ffmpeg arguments, evidence tables, player results, risks |
| [cpu-speed-results.txt](channels/cpu-speed-results.txt) | CPU transcode speed by source class |
| [bench.sh](channels/bench.sh) | Benchmark to run on alpha inside `goshl-ffmpeg:9.0.2-1` |

## Responsibilities

frames owns the channel model, the schedule, the live playlist, the guide, IPTV access and viewer tracking.

goshl owns the channel output format, a deterministic segment plan, segment production, prefetch ahead of the playhead, and keeping channel work isolated from other work.

## Output format

The format is identical for every source, so codec parameters never change at an item boundary. Every item is proven to play through item changes in hls.js and AVFoundation.

- H.264 High, 8-bit, `yuv420p`, at a fixed level and bitrate.
- Scaled to fit 1280x720 with the aspect ratio kept, then padded to exactly 1280x720. SPS, PPS, PAT, PMT and SDT are byte-identical across sources.
- Constant frame rate of 24000/1001: 91% of the library is 23.976 or 24 fps, so 25 or 30 would judder. Uses the `fps` filter with the `-copyts` timeline and without `-enc_time_base demux`. A per-item phase shift is applied for Matroska sources whose timestamps sit off the frame grid.
- AAC-LC stereo at 48 kHz, muxed into the same MPEG-TS segment as the video. The audio track is chosen per item.
- BT.709 colour tags. HDR is tone mapped as in VOD.
- Source metadata stripped with `-map_metadata -1`. Today's VOD output also leaks the source title, so this change applies there too.
- Fixed 48-frame segments of 2.002 s, with forced keyframes. Segment `i` starts at `i × 2.002 s`.

Differences from the proposal, as built: segments are produced in whole grid batches, so each segment is encoded once and joins stay frame-exact; the encoder uses libx264's default threading, so output is not byte-reproducible and nothing depends on it; `ChannelProfile` holds a rational frame rate and frames per segment; channel jobs always encode on the CPU.

## API

- `ChannelProfile` is a value passed per call. `Options.ChannelPoolSize` (0 disables channels) and `Options.ChannelSegmentsPerJob` are set per Controller.
- `ChannelPlan(ctx, source, profile, audioTrack)` returns the media key, the rendition name, the duration and every segment. The plan depends only on the probed duration, so frames can build its schedule before anything is transcoded.
- `ChannelSegment(ctx, source, profile, audioTrack, index)` returns one segment.
- `ChannelPrefetch(ctx, source, profile, audioTrack, from, count)` is non-blocking and idempotent. It reuses the existing flight and job deduplication, so frames can call it every few seconds with overlapping ranges.
- Readiness needs no new API. frames sees `Storage.WriteSegment` and `Coordinator.NotifySegment` for every finished segment.
- Channel segments use the source's media key under a reserved rendition name, so they never collide with VOD renditions and `Invalidate` covers them.

## Isolation and priority

- Channel jobs run in their own lane: a new stream type and a third worker pool. Priority alone cannot preempt a job that is already running.
- `Job.Priority` orders jobs within a lane. It changes the job contract to version 6.
- Decided: worker routing is separated from cache identity. A route profile hashes the job contract version. The cache identity hashes the output contract version. Contract 6 leaves the VOD cache warm.

## CPU cost

Measured on an M3 Pro inside an 11-vCPU Docker VM shared with other work, so the figures are noisy. libx264 superfast at 3 Mb/s to 1280x720, one job:

| Source | Speed | CPU-seconds per content-second |
|---|---|---|
| 1080p H.264 or HEVC | 9.8–10.7x | 0.6–0.7 |
| 1440p | 8.2–8.9x | 0.6–0.8 |
| 4K SDR | 6.0–10.6x | 0.8–1.4 |
| 4K 8-bit PQ, tone mapped | 4.6–5.3x | 1.1–1.3 |
| 4K 10-bit PQ HEVC, tone mapped | 3.7–4.1x | 1.6–1.9 |

- Every class runs faster than real time on this CPU.
- The current tone-mapping chain already scales before converting to linear light. Tone mapping at source resolution would drop to about 1x.
- Alpha's CPU is unknown. Run [bench.sh](channels/bench.sh) on alpha inside the goshl-ffmpeg image. It prints the CPU model, the core count, the cgroup limit and the speed table.

Not yet measured: the preset comparison, concurrent channels on one node, and behaviour under a CPU limit.

## Live sources

Implemented as `Controller.Live`: one ffmpeg per session, a sliding window and an idle stop. The reader comes from the host. Output is H.264 and AAC, copied when possible, not the channel format. See [operations.md](../operations.md#live).

## Open items

- Run `bench.sh` on alpha.
- Players not tested: Safari, iOS, tvOS, ExoPlayer, smart TVs.
- 24.000 fps Matroska sources judder at the phase crossing. A fix needs `ChannelFormatVersion` 2.
- Hardware encoders are unused for channels.
