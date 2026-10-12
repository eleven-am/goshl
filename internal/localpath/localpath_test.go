package localpath

import "testing"

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
		got, local := Resolve(tc.input)
		if local != tc.local || got != tc.want {
			t.Fatalf("Resolve(%q) = %q, %v; want %q, %v", tc.input, got, local, tc.want, tc.local)
		}
	}
}
