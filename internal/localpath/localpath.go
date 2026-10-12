package localpath

import (
	"net/url"
	"strings"
)

func Resolve(sourceURL string) (string, bool) {
	if strings.HasPrefix(sourceURL, "file://") {
		path := strings.TrimPrefix(sourceURL, "file://")
		if strings.HasPrefix(path, "localhost/") {
			path = strings.TrimPrefix(path, "localhost")
		} else if !strings.HasPrefix(path, "/") {
			return "", false
		}
		decoded, err := url.PathUnescape(path)
		return decoded, err == nil
	}
	if strings.HasPrefix(sourceURL, "file:/") {
		decoded, err := url.PathUnescape(strings.TrimPrefix(sourceURL, "file:"))
		return decoded, err == nil
	}
	colon := strings.IndexByte(sourceURL, ':')
	if colon > 0 {
		return "", false
	}
	return sourceURL, true
}
