package engine

import (
	"context"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/hwaccel"
)

func TestIdentityHashesMatchGoldenValues(t *testing.T) {
	if cacheSchemaVersion != "goshl-cache-v6" {
		t.Fatalf("cache schema is %q, want goshl-cache-v6", cacheSchemaVersion)
	}
	accelerated := &domain.HWAccelConfig{
		Accelerator: domain.AccelCUDA,
		DecodeFlags: []string{"-hwaccel", "cuda"},
		EncodeFlags: []string{"-c:v", "h264_nvenc", "-preset", "p4"},
		ScaleFilter: "scale=%d:%d",
		DecodeMode:  domain.DecodeDevice,
	}
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"pipeline identity, accelerated", pipelineIdentity(Config{CacheVersion: "release-7", TargetDuration: 6}, accelerated, 10*time.Second, 5), "goshl:62cbc89505f5e95d30f3282f94dc9ed639c0645545bdb477ac6832761aa87ad7"},
		{"pipeline identity, software", pipelineIdentity(Config{TargetDuration: 4}, &domain.HWAccelConfig{Accelerator: domain.AccelNone, DecodeMode: domain.DecodeSafe}, 1400*time.Millisecond, 1), "goshl:cda7e25164865969fec055e5248992a49a4972536daddb7616c3f84a67cbdb23"},
		{"pipeline version", pipelineVersion(Config{CacheVersion: "release-7", TargetDuration: 6}, hwaccel.NewConfig(domain.AccelNone)), "goshl:9b47f6381c8289283ebb4a87e21ed89596d1cb0d9059cbf94564b550be3eaace"},
		{"media key", hashIdentity(cacheSchemaVersion, "https://media.example/video.mkv", "revision-a", "goshl:pipeline"), "goshl:149bd6d1d5f33110ec38e0cc92071b6852e189fa612333c248b3c5361bf969c1"},
		{"empty identity", hashIdentity(), "goshl:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"split parts", hashIdentity("a", "b"), "goshl:3d64310d8364dfb1b0070f0c7ab813c2ed68ec750463847dbff0a5fc0e9d3af4"},
		{"joined parts", hashIdentity("ab", ""), "goshl:781c03372d8c1c9771f16bcad539bdd70db54c6054ac03a0234f97f93fd438f7"},
		{"sprite asset", assetIdentity("goshl:media", "sprites", struct{ Width, Columns int }{160, 10}), "goshl:media:asset:goshl:a5dd43f1761c67d434cc6773fd4a0fee17486aba89b91c36e113c6f19d2ec2a9"},
		{"subtitle asset", assetIdentity("goshl:media", "subtitle", "subtitle-2"), "goshl:media:asset:goshl:626c52bdd1253eaf5c06fcd6b24dcf311a30bf1c7d89c020fad27306dbe7bfeb"},
		{"subtitle batch asset", assetIdentity("goshl:media", "subtitles", nil), "goshl:media:asset:goshl:9e8ad538186f126c9f18a1021172b9385d73ec98cde6ec71f16e4e4aaba2e622"},
		{"preview asset", assetIdentity("goshl:media", "preview", domain.PreviewConfig{MaxFrames: 40, Frameduration: 250, Width: 480}), "goshl:media:asset:goshl:7a2b72ecdbb440bd173e8147bf924b4f3c305683e2b4bc3bf5ec6d0289ce83c6"},
		{"thumbnail asset", assetIdentity("goshl:media", "thumbnail", domain.ThumbnailConfig{Position: 0.1, Width: 480}), "goshl:media:asset:goshl:e8313c8c870998a8aa7f38ca0d76a48f6f227e30d46adf3f34265deabc825b23"},
		{"video job", deterministicJobID("goshl:media", "1080p", domain.StreamVideo, 4, 7), "1a8d03f41c37a662"},
		{"audio job", deterministicJobID("goshl:media", "aac_stereo", domain.StreamAudio, 0, 3), "9f80d6dc2103ce45"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %s, want %s", c.name, c.got, c.want)
		}
	}
}

func TestEngineIdentitiesMatchGoldenValues(t *testing.T) {
	revision := func(context.Context, string) (string, error) { return "revision-a", nil }
	built := mustNew(Config{Storage: &stubStorage{}, Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}, HWAccelMode: domain.AccelNone, CacheVersion: "release-7", SourceRevision: revision})
	if built.pipelineVersion != "goshl:9b47f6381c8289283ebb4a87e21ed89596d1cb0d9059cbf94564b550be3eaace" {
		t.Errorf("pipeline version = %s", built.pipelineVersion)
	}
	key, err := built.MediaKey(context.Background(), "https://media.example/video.mkv")
	if err != nil || key != "goshl:4156f0d49f186fe08eed0023a012364c1e51c7b20c571518c30c84f137560520" {
		t.Errorf("media key = %s, %v", key, err)
	}
	if got := assetIdentity(key, "sprites", built.miscGen.SpriteIdentity()); got != "goshl:4156f0d49f186fe08eed0023a012364c1e51c7b20c571518c30c84f137560520:asset:goshl:2452e99e4b9de14f37a5f013e31cba337020ae5b12d5975b417fa0c67d8aba6a" {
		t.Errorf("sprite asset key = %s", got)
	}
}
