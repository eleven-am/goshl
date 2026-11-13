package ffmpegversion

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   Version
		err    bool
	}{
		{"plain", "ffmpeg version 8.0.1 Copyright (c) 2000-2025", Version{Major: 8, Minor: 0}, false},
		{"n prefix", "ffmpeg version n6.1.2 Copyright", Version{Major: 6, Minor: 1}, false},
		{"static", "ffmpeg version 6.1-static https://johnvansickle.com", Version{Major: 6, Minor: 1}, false},
		{"homebrew", "ffmpeg version 7.1.1-Homebrew Copyright", Version{Major: 7, Minor: 1}, false},
		{"git master", "ffmpeg version N-112345-g1a2b3c4d5e Copyright", Version{Git: true}, false},
		{"git date", "ffmpeg version git-2024-05-01-abcdef Copyright", Version{Git: true}, false},
		{"ffprobe", "ffprobe version 6.0 Copyright", Version{Major: 6, Minor: 0}, false},
		{"two digit", "ffmpeg version 10.12.3", Version{Major: 10, Minor: 12}, false},
		{"garbage", "hello", Version{}, true},
		{"empty", "", Version{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.output)
			if tc.err {
				if !errors.Is(err, ErrUnrecognized) {
					t.Fatalf("err = %v, want ErrUnrecognized", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestAtLeast(t *testing.T) {
	cases := []struct {
		v    Version
		want bool
	}{
		{Version{Major: 6, Minor: 0}, false},
		{Version{Major: 5, Minor: 9}, false},
		{Version{Major: 6, Minor: 1}, true},
		{Version{Major: 7, Minor: 0}, true},
		{Version{Git: true}, true},
	}
	for _, tc := range cases {
		if got := tc.v.AtLeast(Minimum()); got != tc.want {
			t.Errorf("%+v.AtLeast = %v, want %v", tc.v, got, tc.want)
		}
	}
}

func TestRequire(t *testing.T) {
	write := func(t *testing.T, name, body string) {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}

	t.Run("old", func(t *testing.T) {
		write(t, "fakeold", "#!/bin/sh\necho 'fakeold version 6.0'\n")
		if err := Require(context.Background(), "fakeold", Minimum()); !errors.Is(err, ErrTooOld) {
			t.Fatalf("err = %v, want ErrTooOld", err)
		}
	})
	t.Run("new", func(t *testing.T) {
		write(t, "fakenew", "#!/bin/sh\necho 'fakenew version 7.1.1-Homebrew'\n")
		if err := Require(context.Background(), "fakenew", Minimum()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("silent", func(t *testing.T) {
		write(t, "fakesilent", "#!/bin/sh\nexit 0\n")
		if err := Require(context.Background(), "fakesilent", Minimum()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("missing", func(t *testing.T) {
		if err := Require(context.Background(), "definitely-not-installed-bin", Minimum()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}
