# Changelog

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
