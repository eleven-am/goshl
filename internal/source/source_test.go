package source

import (
	"errors"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	cases := []struct {
		input string
		want  string
		local bool
	}{
		{"/media/a.mp4", "/media/a.mp4", true},
		{"relative/a.mp4", "relative/a.mp4", true},
		{"file:///media/a%20b.mp4", "/media/a b.mp4", true},
		{"file://localhost/media/a.mp4", "/media/a.mp4", true},
		{"file:/media/a.mp4", "/media/a.mp4", true},
		{"file://remote-host/media/a.mp4", "", false},
		{"file:///media/%zz.mp4", "", false},
		{"https://example.com/a.mp4", "", false},
		{"s3://bucket/a.mp4", "", false},
	}
	for _, tc := range cases {
		input, err := Resolve(tc.input)
		got := input.LocalPath
		local := err == nil && !input.Remote
		if local != tc.local || got != tc.want {
			t.Fatalf("Resolve(%q) = %q, %v; want %q, %v", tc.input, got, local, tc.want, tc.local)
		}
	}
}

func TestInputArgsWhitelistsTheInputSide(t *testing.T) {
	args, err := InputArgs("-dash.mp4")
	if err != nil {
		t.Fatalf("InputArgs: %v", err)
	}
	want := []string{"-protocol_whitelist", ProtocolWhitelist, "-i", "file:-dash.mp4"}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args = %v, want %v", args, want)
		}
	}
	if _, err := InputArgs("concat:a|b"); !errors.Is(err, ErrUnsupportedSource) {
		t.Fatalf("concat err = %v", err)
	}
}

func TestResolvePercentEncodedFileURL(t *testing.T) {
	input, err := Resolve("file:///media/My%20Show%3A%20Part%202%25.mkv")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if input.LocalPath != "/media/My Show: Part 2%.mkv" || input.Arg != "file:/media/My Show: Part 2%.mkv" || input.Remote {
		t.Fatalf("input = %+v", input)
	}
	input, err = Resolve("file://localhost/media/a%20b.mp4")
	if err != nil || input.LocalPath != "/media/a b.mp4" {
		t.Fatalf("localhost authority: %+v, %v", input, err)
	}
}

func TestResolveColonInLocalFilename(t *testing.T) {
	for _, name := range []string{"Title: Part 2.mkv", "x:y.mp4", "dir/x:y.mp4", "/abs/Title: Part 2.mkv", "-weird:name.mp4"} {
		input, err := Resolve(name)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", name, err)
		}
		if input.LocalPath != name || input.Arg != "file:"+name || input.Remote {
			t.Fatalf("Resolve(%q) = %+v", name, input)
		}
	}
	input, err := Resolve("./concat:a")
	if err != nil || input.LocalPath != "./concat:a" {
		t.Fatalf("dot-slash escape: %+v, %v", input, err)
	}
	if _, err := Resolve("concat:a"); !errors.Is(err, ErrUnsupportedSource) {
		t.Fatalf("concat:a: err = %v", err)
	}
	if _, err := Resolve("concat:missing|other"); !errors.Is(err, ErrUnsupportedSource) {
		t.Fatalf("missing concat: err = %v", err)
	}
}

func TestResolveRejectsUnsupportedScheme(t *testing.T) {
	for _, raw := range []string{"s3://bucket/a.mp4", "rtmp://host/live", "ftp://host/a.mp4", "tcp://host:1", "file://remote-host/a.mp4", "file:relative.mp4", "file:///a/%zz", "http:///nohost", ""} {
		if _, err := Resolve(raw); !errors.Is(err, ErrUnsupportedSource) {
			t.Fatalf("Resolve(%q) err = %v, want ErrUnsupportedSource", raw, err)
		}
	}
	input, err := Resolve("https://example.com/a.mp4?x=1")
	if err != nil || !input.Remote || input.Arg != "https://example.com/a.mp4?x=1" || input.LocalPath != "" {
		t.Fatalf("https: %+v, %v", input, err)
	}
}

func TestProtocolWhitelistIsExactlyTheSupportedSet(t *testing.T) {
	if ProtocolWhitelist != "file" || RemoteProtocolWhitelist != "http,https,tcp,tls" {
		t.Fatalf("whitelists = %q, %q", ProtocolWhitelist, RemoteProtocolWhitelist)
	}
	args, err := InputArgs("/media/a.mp4")
	if err != nil {
		t.Fatalf("InputArgs: %v", err)
	}
	if len(args) != 4 || args[0] != "-protocol_whitelist" || args[1] != "file" {
		t.Fatalf("args = %v", args)
	}
}

func TestResolveAbsolutePathsNeverReadAsSchemes(t *testing.T) {
	for _, name := range []string{"/http://host/a.mp4", "/concat:a|b", "/tmp/rtmp://x", "/a:b/c.mp4", "/file:/x.mp4"} {
		input, err := Resolve(name)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", name, err)
		}
		if input.Remote || input.LocalPath != name || input.Arg != "file:"+name {
			t.Fatalf("Resolve(%q) = %+v", name, input)
		}
	}
}

func TestInputArgsWhitelistFollowsRemoteness(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"/media/a.mp4", []string{"-protocol_whitelist", "file", "-i", "file:/media/a.mp4"}},
		{"file:///media/a.mp4", []string{"-protocol_whitelist", "file", "-i", "file:/media/a.mp4"}},
		{"https://example.com/a.mp4", []string{"-protocol_whitelist", "http,https,tcp,tls", "-i", "https://example.com/a.mp4"}},
		{"http://example.com/a.mp4", []string{"-protocol_whitelist", "http,https,tcp,tls", "-i", "http://example.com/a.mp4"}},
	}
	for _, tc := range cases {
		args, err := InputArgs(tc.raw)
		if err != nil {
			t.Fatalf("InputArgs(%q): %v", tc.raw, err)
		}
		if strings.Join(args, "\x00") != strings.Join(tc.want, "\x00") {
			t.Fatalf("InputArgs(%q) = %q, want %q", tc.raw, args, tc.want)
		}
	}
}

func TestRemoteInputsNeverWhitelistFile(t *testing.T) {
	args, err := InputArgs("https://example.com/a.mp4")
	if err != nil {
		t.Fatalf("InputArgs: %v", err)
	}
	for _, protocol := range strings.Split(args[1], ",") {
		if protocol == "file" {
			t.Fatalf("remote whitelist %q contains file", args[1])
		}
	}
}
