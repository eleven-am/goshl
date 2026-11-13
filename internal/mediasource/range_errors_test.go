package mediasource

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestHTTPReaderSeparatesServerFailuresFromMissingRangeSupport(t *testing.T) {
	content := []byte("0123456789")
	cases := map[string]struct {
		status      int
		unsupported bool
	}{
		"server error":        {http.StatusInternalServerError, false},
		"unavailable":         {http.StatusServiceUnavailable, false},
		"gone":                {http.StatusNotFound, false},
		"forbidden":           {http.StatusForbidden, false},
		"range no longer met": {http.StatusOK, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if requests.Add(1) == 1 {
					writer.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(content)))
					writer.WriteHeader(http.StatusPartialContent)
					_, _ = writer.Write(content[:1])
					return
				}
				writer.WriteHeader(tc.status)
				_, _ = writer.Write(content)
			}))
			defer server.Close()

			reader, err := Open(t.Context(), server.URL+"/media.mp4")
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer reader.Close()
			_, err = reader.ReadAt(make([]byte, 4), 2)
			if err == nil {
				t.Fatal("read succeeded against a failing server")
			}
			if got := errors.Is(err, ErrUnsupported); got != tc.unsupported {
				t.Fatalf("status %d: ErrUnsupported = %v, want %v (%v)", tc.status, got, tc.unsupported, err)
			}
		})
	}
}
