package rendition

import "strings"

const maxNameLength = 64

func ValidName(name string) bool {
	if name == "" || len(name) > maxNameLength || name[0] == '.' || strings.Contains(name, "..") {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}
