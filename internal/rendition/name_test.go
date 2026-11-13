package rendition

import (
	"strings"
	"testing"
)

func TestValidName(t *testing.T) {
	valid := []string{"720p", "aac_stereo", "audio1_aac_surround", "eac3_passthrough", "a-b.c", "1080p"}
	for _, name := range valid {
		if !ValidName(name) {
			t.Errorf("ValidName(%q) = false, want true", name)
		}
	}
	invalid := []string{"", ".hidden", "..", "a..b", "../etc/passwd", "a/b", `a\b`, "a b", "a\x00b", "a%2fb", "é", strings.Repeat("a", maxNameLength+1)}
	for _, name := range invalid {
		if ValidName(name) {
			t.Errorf("ValidName(%q) = true, want false", name)
		}
	}
}

func TestGeneratedNamesAreValid(t *testing.T) {
	for _, name := range []string{"720p", "aac_stereo", "audio12_aac_surround", "truehd_passthrough"} {
		if !ValidName(name) {
			t.Errorf("generated-style name %q rejected", name)
		}
	}
}
