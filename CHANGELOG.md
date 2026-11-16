# Changelog

## [0.0.29] - 2026-11-16

Cache `goshl-cache-v6` unchanged; job contract 6. See MIGRATION.md.

- feat(live): `Controller.Live` and `LiveProbe` serve a raw MPEG-TS reader as sliding-window HLS; H.264 and AAC are copied, HEVC, MPEG-2 and other audio are transcoded; reconnects and `SwitchSource` keep timestamps rising with `EXT-X-DISCONTINUITY`; idle sessions stop; events carry only the `Label`
- feat(channels): `ChannelPlan`, `ChannelSegment` and `ChannelPrefetch` produce linear TV segments in one fixed format (24000/1001 fps, 1280x720 padded, muxed AAC, 2.002 s segments) in their own `StreamChannel` lane
- feat(contract): job contract 6 adds `Job.Priority` and `Job.Channel`; the digest covers both; coordinators must round-trip them and deliver higher priority first
- feat(cache): jobs route on a route profile that includes the job contract version; cache identity hashes the output contract version (5), so the VOD cache stays warm
- feat(slots): `Options.EncoderSlots` is a shared budget for VOD video transcodes and live transcodes; `LiveMaxSessions` and `LiveMaxTranscodes` cap live
- feat(options): `ChannelPoolSize`, `ChannelSegmentsPerJob`, `EncoderSlots`, `LiveMaxSessions`, `LiveMaxTranscodes`; all default to off or to values that change nothing for VOD
- feat(events): `EventLive*` and `Event.Label`
- fix(vod): VOD segments no longer carry the source title or other metadata (`-map_metadata -1 -map_chapters -1`)

## [0.0.28] - 2026-11-13

No behaviour, API, cache or job contract change.

- refactor: the root package holds only the public API; scheduling, flights, metadata, identity and asset machinery live in an internal engine package
- test: end-to-end tests are a black-box e2e package grouped by concern; unit tests moved with their code, none removed
- docs: shorter README; API reference, configuration, operations and architecture in docs/; MIGRATION rewritten as per-version upgrade steps

## [0.0.27] - 2026-11-13

Cache `goshl-cache-v6`; job contract version unchanged (5). See MIGRATION.md.

- fix(colour): PQ and HLG sources are tone mapped at any bit depth, including 8-bit, when their primaries are BT.2020 or unspecified (assumed BT.2020); v0.0.26 retagged every 8-bit PQ or HLG source as BT.709 without tone mapping, which washed out genuinely PQ-graded 8-bit files
- fix(colour): PQ or HLG with BT.709, SMPTE 170M or BT.470 (BG or M) primaries is treated as mis-tagged SDR and retagged BT.709 at any bit depth (previously only below 10-bit)
- feat(cache): cache schema `goshl-cache-v6`; every source is re-probed and re-transcoded once

## [0.0.26] - 2026-11-13

Output correctness release R2 (cache `goshl-cache-v5`, job contract version 5). See MIGRATION.md.

- feat(planner): segments last at most max(2 x target, 12 s); long keyframe gaps are split, tail slivers merge, rounding noise no longer merges segments
- feat(probe): `Metadata.Bitrate`, `VideoStream.End` and `AudioStream.End`; ends are measured from the file tail when undeclared; average frame rate; MKV and WebM keyframes from Matroska Cues; MP4 header I/O errors fail the probe
- fix(audio): every audio frame belongs to exactly one job at a join (AAC grid-owned cuts, trailing segment for audio jobs, AC-3 passthrough join)
- fix(video): parameter-set-only access units stripped from copied avc1 segments
- fix(video): SEI stripped from copied H.264 without closed captions, fixing Safari rejecting sources with an empty SEI
- fix(playlist): AVERAGE-BANDWIDTH reflects the real mean segment bitrate; HDR sources without zscale fail the master and video variant playlists with ErrTonemapUnavailable
- fix(colour): 8-bit sources tagged PQ or HLG are treated as mis-tagged SDR
- feat(colour): all transcodes tagged BT.709 limited range; PQ and HLG sources tone mapped with `zscale` and `tonemap` (an ffmpeg with libzimg is required for HDR); HDR is not direct-streamed
- feat(hwaccel): auto-detection validates CUDA, QSV, VideoToolbox and VAAPI in order, reports skipped accelerators in `HWAccelConfig().Error` and `EventHardwareFallback`; VideoToolbox uses `-g 250`; software preset is `superfast` (High profile)
- feat(playlist): RFC 8216 audio groups, RFC 5646 languages, unique subtitle names, valid audio-only masters; invalid playlists return errors
- feat(subtitles): only text codecs are listed and served, others return `ErrRenditionNotFound`; MKV subtitles read only cued blocks; failed batches are not rerun identically
- feat(video): a video that ends early repeats its last frame (`VideoRendition.SourceEnd`); renditions above H.264 level 5.2 are dropped with the new `EventRenditionDropped`
- feat(contract): job contract 5: coordinators must also carry `Job.Digest`, `VideoRendition.BufferSize`, `PeakBandwidth`, `Colour` and `SourceEnd`, and `AudioRendition.SampleRate`, `PeakBandwidth`, `SourceEnd` and `Title`; workers verify the digest and reject a job that changed in transit; `ColourPlan` is exported; `AudioRendition.GroupID` and `DirectPlay` are removed
- fix(audio): non-48 kHz audio joins frame-exactly across jobs; job output is validated against the plan
- fix(runtime): `Start` returns `ErrControllerStopped` after `Stop`; lock-held waiters, enqueue cancellation and pool shutdown with a cause no longer orphan locks or fail redeliverable jobs
- fix(options): `TargetDuration` 1-60 s, `SegmentsPerJob` at most 1000, pool sizes at most 1024; inputs restricted with `-format_whitelist`; `ErrUnsupportedSource` for empty, unsupported or malformed sources
- docs: trust model for source URLs, ffmpeg 6.1 startup check, HDR requirement

## [0.0.22] - 2026-10-20

- fix(transcode): measure the seek landing for direct-stream jobs so copied segments align with the playlist


## [0.0.21] - 2026-10-20

- fix(misc): subtitle batches survive caller cancel, lock waiters take over, configurable generation timeout


## [0.0.20] - 2026-10-19

- perf: extract progressive MP4 text subtitles from indexed byte ranges when first requested
- feat: support indexed `mov_text`/`tx3g` and `wvtt` conversion to cached WebVTT
- fix: retain the FFmpeg subtitle path for unsupported formats and layouts

## [0.0.19] - 2026-10-18

- fix: scope distributed job completion and settlement to unique runs and delivery receipts
- fix: renew worker delivery leases, atomically settle terminal work, and redeliver interrupted jobs
- fix: keep cluster work alive when only one replica loses its local waiters
- fix: route workers by pipeline profile and replay terminal failures to segment callers

## [0.0.18] - 2026-10-17

- feat: add Controller.SubtitlesVTT to extract several subtitle tracks in one ffmpeg invocation
- fix: reject bitmap subtitle codecs per track and honour cancellation when extracting subtitles

## [0.0.17] - 2026-10-17

- fix: keep a job's lock until the job finishes and let later segment requests wait on it
- fix: anchor transcode jobs at the requested segment without overlapping neighbouring jobs
- feat: cancel jobs whose last waiter left after AbandonGrace
- feat: add job.joined and job.abandoned events and segment ready latency

## [0.0.16] - 2026-10-13

- fix: derive valid H.264 levels for renditions


## [0.0.15] - 2026-10-12

- feat: read MP4 keyframes from the moov header


## [0.0.14] - 2026-10-11

- chore: verify the tree before tagging a release
- feat: add ProbeTimeout and SpriteStorage options
- feat: generate sprite sheets from keyframes only


## [0.0.13] - 2026-08-04

- Use NVENC-supported frame dimensions for the functional hardware pipeline self-test

## [0.0.12] - 2026-08-04

- Normalize non-zero media timestamps and preserve the complete playback timeline
- Add versioned source, pipeline, and generated-asset cache identities with optional invalidation
- Deduplicate segment batches and generated assets across replicas, with durable-storage polling for lost notifications
- Add strict job contracts, output validation, deadlines, typed request errors, and structured observer events
- Add runtime hardware pipeline validation, safe CUDA decoding, explicit device-frame mode, and visible fallback behavior
- Generate codec-aware HLS masters for silent, audio-only, multi-codec audio, and subtitle media
- Tighten browser direct-stream compatibility and force transcoded H.264 to High-profile `yuv420p`
- Bound FFmpeg output, redact media credentials, sparsely sample previews, and seek thumbnails directly
- Update `golang.org/x/image` and add real FFmpeg regression coverage

## [0.0.8] - 2025-12-14




## [0.0.7] - 2025-12-14

- Implement distributed locking for segment and metadata retrieval


## [0.0.6] - 2025-12-13

- Add public Metadata method to Controller


## [0.0.5] - 2025-12-13

- Add thumbnail generation from sprite sheets


## [0.0.4] - 2025-12-13

- Add animated GIF preview generation from sprite sheets


## [0.0.3] - 2025-12-04

- Add SegmentStatus type to controller for segment processing state


## [0.0.2] - 2025-12-04

- Add Makefile for version management and release automation


## [0.0.1] - 2025-12-04

- Initial commit
