# Configuration

```go
controller, err := goshl.NewControllerWithContext(ctx, goshl.Options{
	Storage:     storage,
	Coordinator: coordinator,
	PathGen:     pathGen,
	HWAccelMode: goshl.AccelCUDA,
	JobTimeout:  3 * time.Minute,
})
```

A zero value takes the default. A negative value is an error.

## Options

| Field | Type | Default | Bounds | Meaning |
|---|---|---|---|---|
| `Storage` | `Storage` | required | non-nil | Persists metadata, segments, sprites, subtitles, previews and thumbnails. |
| `Coordinator` | `Coordinator` | required | non-nil | Distributes jobs, notifies segments, holds locks. |
| `PathGen` | `PathGenerator` | required | non-nil | Builds the URLs embedded in playlists. |
| `SpriteStorage` | `SpriteStorage` | nil | | Stores sprite sheets instead of `Storage`. |
| `HWAccel` | `bool` | `false` | | Auto-detects a hardware accelerator. |
| `HWAccelMode` | `Accelerator` | `""` | `AccelNone`, `AccelCUDA`, `AccelVideoToolbox`, `AccelVAAPI`, `AccelQSV` | Selects one accelerator and overrides `HWAccel`. |
| `HWAccelDecodeMode` | `DecodeMode` | `DecodeSafe` | `DecodeSafe`, `DecodeDevice` | CUDA decode path. |
| `AllowHardwareFallback` | `bool` | `false` | | Falls back to software when an explicit `HWAccelMode` fails validation. |
| `SegmentTimeout` | `time.Duration` | 30s | > 0 | Longest wait of one `Segment` call. |
| `JobTimeout` | `time.Duration` | 2m | > 0 | Execution budget of one ffmpeg job. Also the queue wait allowed before `StartBy`. |
| `ProbeTimeout` | `time.Duration` | `JobTimeout` | > 0 | Bound on one ffprobe run. |
| `GenerationTimeout` | `time.Duration` | 10m | > 0 | Bound on sprites, previews, thumbnails and subtitles. |
| `TargetDuration` | `float64` | 6.0 | 1 to 60 seconds, finite | Target segment length. |
| `SegmentsPerJob` | `int` | 4 | 1 to 1000 | Segments produced by one job. |
| `VideoPoolSize` | `int` | 2 | 1 to 1024 | Video workers. |
| `AudioPoolSize` | `int` | 4 | 1 to 1024 | Audio workers. |
| `DefaultPreviewConfig.MaxFrames` | `int` | 40 | 1 to 1000 | Frames in a preview. |
| `DefaultPreviewConfig.Frameduration` | `int` | 250 | 1 to 60000 | Milliseconds per preview frame. |
| `DefaultPreviewConfig.Width` | `int` | 480 | 1 to 4096 | Preview width in pixels. |
| `DefaultThumbnailConfig.Position` | `float64` | 0.1 | greater than 0, up to 1 | Thumbnail position as a fraction of the duration. |
| `DefaultThumbnailConfig.Width` | `int` | 480 | 1 to 8192 | Thumbnail width in pixels. |
| `OmitSubtitleRenditions` | `bool` | `false` | | Leaves subtitle groups out of the master playlist. |
| `CacheVersion` | `string` | `""` | | Host-side cache salt. Changing it starts a new cache namespace. |
| `SourceRevision` | `SourceRevisionResolver` | nil | | Returns the revision of a source for the media key. |
| `Observer` | `Observer` | nil | | Receives lifecycle events. |

## Resolution order

`NewControllerWithContext` runs these steps and stops at the first failure.

1. Reject nil `Storage`, `Coordinator` or `PathGen`.
2. Reject negative durations, non-finite `TargetDuration`, negative sizes, out-of-range preview or thumbnail values, and an unknown `HWAccelDecodeMode`.
3. Fill zero fields with defaults. `ProbeTimeout` copies the resolved `JobTimeout`.
4. Re-check the resolved values: positive durations and sizes, `TargetDuration` 1 to 60, `SegmentsPerJob` at most 1000, pool sizes at most 1024, preview and thumbnail ranges.
5. Check the ffmpeg and ffprobe versions.
6. Select the hardware configuration.

Every failure in steps 1 to 5 wraps `ErrInvalidOptions`. Preview and thumbnail failures also wrap `ErrInvalidPreviewConfig` or `ErrInvalidThumbnailConfig`. Both are reachable with `errors.Is`.

## Preview and thumbnail values

`Preview(ctx, src, nil)` and `Thumbnail(ctx, src, nil)` use the defaults. A non-nil config with zero fields falls back to the default for each zero field. Out-of-range values return `ErrInvalidPreviewConfig` or `ErrInvalidThumbnailConfig`.

`Position` 0 means the default, so the first frame is not selectable. Use `0.001` for the earliest frame.

## Timing

| Value | Derived from |
|---|---|
| `StartBy` of a job | enqueue time + `JobTimeout` |
| Local flight lifetime | `StartBy` + `JobTimeout` |
| Job lock TTL | `StartBy` + `JobTimeout` + 1 minute |
| Segment length cap | `max(2 x TargetDuration, 12 s)` |

## Source revision

A local file uses its size and modification time as its revision. A remote URL uses the full URL. Return an ETag, version or content fingerprint from `SourceRevision` so a refreshed signed URL keeps its cache.

```go
Options{
	SourceRevision: func(ctx context.Context, sourceURL string) (string, error) {
		return mediaVersion(ctx, sourceURL)
	},
}
```
