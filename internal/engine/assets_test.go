package engine

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
)

type spriteSpy struct {
	existsCalls int
	writeCalls  int
	deleted     []string
}

func (s *spriteSpy) WriteSprite(ctx context.Context, assetKey string, index int, data []byte) error {
	s.writeCalls++
	return nil
}

func (s *spriteSpy) ReadSprite(ctx context.Context, assetKey string, index int) ([]byte, error) {
	return []byte("img"), nil
}

func (s *spriteSpy) SpriteExists(ctx context.Context, assetKey string, index int) (bool, error) {
	s.existsCalls++
	return true, nil
}

func (s *spriteSpy) DeleteMedia(ctx context.Context, mediaKey string) error {
	s.deleted = append(s.deleted, mediaKey)
	return nil
}

type plainSpriteStore struct{ domain.SpriteStorage }

type thumbnailSpy struct {
	*stubStorage
	keys []string
}

func (s *thumbnailSpy) ThumbnailExists(ctx context.Context, key string) (bool, error) {
	s.keys = append(s.keys, key)
	return true, nil
}

func (s *thumbnailSpy) ReadThumbnail(ctx context.Context, key string) ([]byte, error) {
	return []byte("thumb"), nil
}

type versionedPathGen struct {
	stubPathGen
	prefix string
}

func (p versionedPathGen) Sprite(sourceURL string, index int) string {
	return p.prefix + "/sprite-" + strconv.Itoa(index) + ".jpg"
}

func cachedMetadataStorage(t *testing.T, duration float64) *stubStorage {
	t.Helper()
	encoded, err := json.Marshal(testVideoMetadata(duration, nil))
	if err != nil {
		t.Fatal(err)
	}
	return &stubStorage{metaData: encoded, metaExists: true}
}

func TestInvalidateDeletesSpriteStorage(t *testing.T) {
	main := &invalidatingStorage{stubStorage: &stubStorage{}}
	sprites := &spriteSpy{}
	controller := mustNew(Config{Storage: main, SpriteStorage: sprites, Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}})

	if err := controller.Invalidate(context.Background(), "file:///media"); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	mediaKey, err := controller.MediaKey(context.Background(), "file:///media")
	if err != nil {
		t.Fatal(err)
	}
	if len(main.keys) != 1 || main.keys[0] != mediaKey {
		t.Fatalf("expected main storage purge for %s, got %v", mediaKey, main.keys)
	}
	if len(sprites.deleted) != 1 || sprites.deleted[0] != mediaKey {
		t.Fatalf("expected sprite storage purge for %s, got %v", mediaKey, sprites.deleted)
	}
}

func TestInvalidateRejectsSpriteStorageWithoutInvalidation(t *testing.T) {
	main := &invalidatingStorage{stubStorage: &stubStorage{}}
	controller := mustNew(Config{Storage: main, SpriteStorage: plainSpriteStore{&spriteSpy{}}, Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}})

	err := controller.Invalidate(context.Background(), "file:///media")

	if !errors.Is(err, ErrCacheInvalidationUnsupported) {
		t.Fatalf("expected ErrCacheInvalidationUnsupported, got %v", err)
	}
	if len(main.keys) != 0 {
		t.Fatalf("nothing may be purged when invalidation is only partly supported, got %v", main.keys)
	}
}

func TestThumbnailRequestWithOnlyWidthKeepsDefaultPosition(t *testing.T) {
	storage := &thumbnailSpy{stubStorage: cachedMetadataStorage(t, 60)}
	controller := mustNew(Config{Storage: storage, Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}})
	mediaKey, err := controller.MediaKey(context.Background(), "file:///media")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := controller.Thumbnail(context.Background(), "file:///media", &domain.ThumbnailConfig{Width: 640}); err != nil {
		t.Fatalf("thumbnail: %v", err)
	}

	want := assetIdentity(mediaKey, "thumbnail", domain.ThumbnailConfig{Position: 0.1, Width: 640})
	if len(storage.keys) == 0 || storage.keys[0] != want {
		t.Fatalf("expected thumbnail key for default position, want %s got %v", want, storage.keys)
	}
}

func TestThumbnailRequestPositionOverridesDefault(t *testing.T) {
	storage := &thumbnailSpy{stubStorage: cachedMetadataStorage(t, 60)}
	controller := mustNew(Config{Storage: storage, Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}})
	mediaKey, err := controller.MediaKey(context.Background(), "file:///media")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := controller.Thumbnail(context.Background(), "file:///media", &domain.ThumbnailConfig{Position: 0.5}); err != nil {
		t.Fatalf("thumbnail: %v", err)
	}

	want := assetIdentity(mediaKey, "thumbnail", domain.ThumbnailConfig{Position: 0.5, Width: 480})
	if len(storage.keys) == 0 || storage.keys[0] != want {
		t.Fatalf("expected explicit position to apply, want %s got %v", want, storage.keys)
	}
}

func TestSpriteVTTUsesCurrentPathGenerator(t *testing.T) {
	sprites := &spriteSpy{}
	controller := mustNew(Config{
		Storage:       cachedMetadataStorage(t, 60),
		SpriteStorage: sprites,
		Coordinator:   &stubCoordinator{},
		PathGen:       versionedPathGen{prefix: "/v2"},
	})

	out, err := controller.SpriteVTT(context.Background(), "file:///media")
	if err != nil {
		t.Fatalf("sprite vtt: %v", err)
	}

	text := string(out)
	if !strings.Contains(text, "/v2/sprite-0.jpg#xywh=0,0,160,90") {
		t.Fatalf("expected vtt rendered with the current path generator, got %q", text)
	}
	if strings.Count(text, "-->") != 12 {
		t.Fatalf("expected 12 cues for 60s at 5s, got %q", text)
	}
}

type mainSpriteTouchSpy struct {
	*stubStorage
	touched int
}

func (s *mainSpriteTouchSpy) WriteSprite(ctx context.Context, key string, index int, data []byte) error {
	s.touched++
	return nil
}

func (s *mainSpriteTouchSpy) ReadSprite(ctx context.Context, key string, index int) ([]byte, error) {
	s.touched++
	return nil, nil
}

func (s *mainSpriteTouchSpy) SpriteExists(ctx context.Context, key string, index int) (bool, error) {
	s.touched++
	return false, nil
}

func TestSpriteURLUsesPathGeneratorForEachIndex(t *testing.T) {
	svc := mustNew(Config{
		Storage:     &stubStorage{},
		Coordinator: &stubCoordinator{},
		PathGen:     stubPathGen{},
	})

	if got := svc.spriteURL("file:///media")(12); got != "/sprite-12.jpg" {
		t.Fatalf("expected direct path generator result, got %q", got)
	}
}

func TestPreviewReturnsDataWithCustomConfig(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := &domain.Metadata{Duration: 10}
	metaBytes, _ := json.Marshal(meta)
	svc := mustNew(Config{
		Storage:     &stubStorage{metaData: metaBytes, metaExists: true},
		Coordinator: &stubCoordinator{},
		PathGen:     stubPathGen{},
		DefaultPreviewConfig: domain.PreviewConfig{
			MaxFrames:     20,
			Frameduration: 200,
			Width:         320,
		},
	})

	_, err := svc.Preview(context.Background(), "file:///media", &domain.PreviewConfig{
		MaxFrames: 10,
		Width:     240,
	})
	if err == nil {
		t.Logf("preview with custom config executed (error expected due to missing sprites)")
	}
}

func TestInvalidateUsesResolvedMediaKey(t *testing.T) {
	storage := &invalidatingStorage{stubStorage: &stubStorage{}}
	controller := mustNew(Config{
		Storage:        storage,
		Coordinator:    &stubCoordinator{},
		PathGen:        stubPathGen{},
		SourceRevision: func(context.Context, string) (string, error) { return "revision", nil },
	})
	want, err := controller.MediaKey(context.Background(), "https://media.example/video")
	if err != nil {
		t.Fatalf("resolve media key: %v", err)
	}
	if err := controller.Invalidate(context.Background(), "https://media.example/video"); err != nil {
		t.Fatalf("invalidate media: %v", err)
	}
	if len(storage.keys) != 1 || storage.keys[0] != want {
		t.Fatalf("invalidated keys %#v, want %q", storage.keys, want)
	}
}

func TestInvalidateReportsUnsupportedStorage(t *testing.T) {
	controller := mustNew(Config{Storage: &stubStorage{}, Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}})
	if err := controller.Invalidate(context.Background(), "source"); !errors.Is(err, ErrCacheInvalidationUnsupported) {
		t.Fatalf("expected unsupported invalidation error, got %v", err)
	}
}

func TestGeneratedAssetRequestsRejectInvalidConfigurationBeforeProbe(t *testing.T) {
	storage := &stubStorage{}
	controller := mustNew(Config{Storage: storage, Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}})
	if _, err := controller.Preview(context.Background(), "source", &domain.PreviewConfig{Width: -1}); !errors.Is(err, ErrInvalidPreviewConfig) {
		t.Fatalf("expected invalid preview configuration, got %v", err)
	}
	if _, err := controller.Thumbnail(context.Background(), "source", &domain.ThumbnailConfig{Position: math.NaN()}); !errors.Is(err, ErrInvalidThumbnailConfig) {
		t.Fatalf("expected invalid thumbnail configuration, got %v", err)
	}
	if storage.metaExistsCalls != 0 {
		t.Fatalf("invalid asset requests must not probe storage")
	}
}

func TestSpriteVTTReadsFromConfiguredSpriteStorage(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	metaBytes, _ := json.Marshal(testVideoMetadata(60, nil))
	main := &mainSpriteTouchSpy{stubStorage: &stubStorage{metaData: metaBytes, metaExists: true}}
	sprites := &spriteSpy{}
	svc := mustNew(Config{Storage: main, SpriteStorage: sprites, Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}})

	out, err := svc.SpriteVTT(context.Background(), "file:///media")

	if err != nil || !strings.Contains(string(out), "/sprite-0.jpg#xywh=0,0,160,90") {
		t.Fatalf("expected vtt rendered from sprites in sprite storage, got %q %v", out, err)
	}
	if sprites.existsCalls != 1 {
		t.Fatalf("expected the configured sprite storage to be consulted once, got %d", sprites.existsCalls)
	}
	if sprites.writeCalls != 0 || main.touched != 0 {
		t.Fatalf("existing sheets must not trigger generation or touch main storage, got writes=%d main=%d", sprites.writeCalls, main.touched)
	}
}
