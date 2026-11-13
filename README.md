# goshl

goshl is a Go library that serves video files as HLS streams. It probes a source, builds master and variant playlists, and transcodes segments with ffmpeg only when a client asks for them. Finished segments are cached, so playback starts before a long file is fully transcoded. A coordinator lets several replicas share one queue, one cache and one set of locks.

## Install

```bash
go get github.com/eleven-am/goshl
```

## Quick start

You supply three things: a `Storage`, a `Coordinator` and a `PathGenerator`. See [docs/api.md](docs/api.md) for their contracts.

```go
package main

import (
	"context"
	"io"
	"log"
	"net/http"

	"github.com/eleven-am/goshl"
)

func main() {
	ctx := context.Background()
	controller, err := goshl.NewControllerWithContext(ctx, goshl.Options{
		Storage:     newStorage(),
		Coordinator: newCoordinator(),
		PathGen:     newPathGen(),
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := controller.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer controller.Stop()

	http.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		playlist, err := controller.MasterPlaylist(r.Context(), r.URL.Query().Get("src"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		io.WriteString(w, playlist)
	})
	log.Fatal(http.ListenAndServe(":8080", nil))
}
```

`newStorage`, `newCoordinator` and `newPathGen` are your implementations. The `PathGenerator` returns URLs that route to handlers calling `VariantPlaylist`, `Segment`, `SubtitlePlaylist`, `SubtitleVTT` and `Sprite`.

## Requirements

- `ffmpeg` and `ffprobe` 6.1 or newer in `PATH`. The constructor checks both at startup and returns an error wrapping `ErrInvalidOptions` for older or unrecognised versions.
- An ffmpeg with the `zscale` and `tonemap` filters for HDR sources. It is checked on first use.
- The same ffmpeg build on every replica.
- Sources are local paths, `file:`, `http:` or `https:` URLs.

## ffmpeg image

| Image | ffmpeg | Platforms | Digest |
|---|---|---|---|
| `ghcr.io/eleven-am/goshl-ffmpeg:9.0.2-1` | 9.0.2 | linux/amd64, linux/arm64 | `sha256:49618bfc33fd0a8e9705be5aae97129172b596bc3a1b69a9fd03664a2ccfcaef` |

## Documentation

- [docs/api.md](docs/api.md): every exported type, function, method, event and error.
- [docs/configuration.md](docs/configuration.md): `Options` fields, defaults and bounds.
- [docs/operations.md](docs/operations.md): ffmpeg, hardware, HDR, caching, coordinator contract, trust model, observability.
- [docs/architecture.md](docs/architecture.md): pipeline and multi-replica coordination.
- [MIGRATION.md](MIGRATION.md): upgrade steps per version.
- [CHANGELOG.md](CHANGELOG.md): release history.

## License

GPL-3.0
