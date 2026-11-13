package mediasource

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/eleven-am/goshl/internal/source"
)

func TestHTTPReaderUsesExactByteRanges(t *testing.T) {
	content := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		rangeHeader := request.Header.Get("Range")
		mu.Lock()
		requests = append(requests, rangeHeader)
		mu.Unlock()

		start, end, ok := parseRequestRange(rangeHeader)
		if !ok || start < 0 || end >= int64(len(content)) {
			http.Error(writer, "invalid range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = writer.Write(content[start : end+1])
	}))
	defer server.Close()

	reader, err := Open(t.Context(), server.URL+"/media.mp4")
	if err != nil {
		t.Fatalf("open HTTP source: %v", err)
	}
	defer reader.Close()
	if reader.Size() != int64(len(content)) {
		t.Fatalf("size = %d, want %d", reader.Size(), len(content))
	}

	buffer := make([]byte, 7)
	n, err := reader.ReadAt(buffer, 11)
	if err != nil {
		t.Fatalf("read range: %v", err)
	}
	if n != len(buffer) || string(buffer) != string(content[11:18]) {
		t.Fatalf("read = %q (%d), want %q (%d)", buffer, n, content[11:18], len(buffer))
	}

	mu.Lock()
	got := append([]string(nil), requests...)
	mu.Unlock()
	want := []string{"bytes=0-0", "bytes=11-17"}
	if len(got) != len(want) {
		t.Fatalf("range requests = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("range request %d = %q, want %q", index, got[index], want[index])
		}
	}
}

func TestHTTPReaderReturnsEOFForPartialTailRead(t *testing.T) {
	content := []byte("0123456789")
	server := newRangeServer(content)
	defer server.Close()

	reader, err := Open(t.Context(), server.URL)
	if err != nil {
		t.Fatalf("open HTTP source: %v", err)
	}
	defer reader.Close()

	buffer := make([]byte, 5)
	n, err := reader.ReadAt(buffer, 8)
	if err != io.EOF {
		t.Fatalf("read error = %v, want EOF", err)
	}
	if n != 2 || string(buffer[:n]) != "89" {
		t.Fatalf("read = %q (%d), want %q (2)", buffer[:n], n, "89")
	}
}

func TestHTTPReaderRejectsServersWithoutRangeSupport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte("whole file"))
	}))
	defer server.Close()

	_, err := Open(t.Context(), server.URL)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("open error = %v, want unsupported", err)
	}
}

func newRangeServer(content []byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		start, end, ok := parseRequestRange(request.Header.Get("Range"))
		if !ok || start < 0 || end >= int64(len(content)) {
			http.Error(writer, "invalid range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = writer.Write(content[start : end+1])
	}))
}

func parseRequestRange(value string) (int64, int64, bool) {
	if !strings.HasPrefix(value, "bytes=") {
		return 0, 0, false
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes="), "-")
	if len(parts) != 2 {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	end, err := strconv.ParseInt(parts[1], 10, 64)
	return start, end, err == nil && start <= end
}

func TestRemoteClientRejectsCrossSchemeRedirect(t *testing.T) {
	for _, target := range []string{"file:///etc/hosts", "ftp://example.com/a.mp4", "gopher://example.com/"} {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			http.Redirect(writer, request, target, http.StatusFound)
		}))
		_, err := Open(t.Context(), server.URL)
		server.Close()
		if !errors.Is(err, errRedirectRejected) {
			t.Fatalf("redirect to %s: err = %v, want errRedirectRejected", target, err)
		}
	}
}

func TestRemoteClientRejectsHTTPSDowngradeAndRedirectLoops(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {}))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, plain.URL, http.StatusFound)
	}))
	defer secure.Close()
	client := newRemoteClient()
	client.Transport = secure.Client().Transport
	if _, err := client.Get(secure.URL); !errors.Is(err, errRedirectRejected) {
		t.Fatalf("downgrade err = %v", err)
	}

	loop := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, request.URL.Path+"x", http.StatusFound)
	}))
	defer loop.Close()
	if _, err := Open(t.Context(), loop.URL); !errors.Is(err, errRedirectRejected) {
		t.Fatalf("loop err = %v", err)
	}
}

func TestMissingLocalSourceIsNotExist(t *testing.T) {
	_, err := Open(t.Context(), "/definitely/not/here.mp4")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want os.ErrNotExist", err)
	}
	if errors.Is(err, ErrUnsupported) {
		t.Fatalf("missing file must not be ErrUnsupported: %v", err)
	}
}

func TestUnsupportedSourceMatchesBothSentinels(t *testing.T) {
	for _, raw := range []string{"s3://bucket/a.mp4", "tcp://host:1", "concat:a|b", ""} {
		_, err := Open(t.Context(), raw)
		if !errors.Is(err, ErrUnsupported) || !errors.Is(err, source.ErrUnsupportedSource) {
			t.Fatalf("Open(%q) err = %v, want both ErrUnsupported and source.ErrUnsupportedSource", raw, err)
		}
	}
}

func TestRemoteSourcesShareOneHTTPClient(t *testing.T) {
	server := newRangeServer([]byte("0123456789"))
	defer server.Close()

	first, err := Open(t.Context(), server.URL)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer first.Close()
	second, err := Open(t.Context(), server.URL)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer second.Close()

	left, right := first.(*httpReader), second.(*httpReader)
	if left.client != right.client || left.client.Transport != right.client.Transport {
		t.Fatal("remote sources must share one client and transport so idle connections are reused")
	}
}

func TestRedirectLimitIsExactlyFive(t *testing.T) {
	content := []byte("0123456789")
	build := func() *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			var remaining int
			if _, err := fmt.Sscanf(request.URL.Path, "/hop/%d", &remaining); err == nil && remaining > 0 {
				http.Redirect(writer, request, fmt.Sprintf("/hop/%d", remaining-1), http.StatusFound)
				return
			}
			start, end, ok := parseRequestRange(request.Header.Get("Range"))
			if !ok || end >= int64(len(content)) {
				http.Error(writer, "invalid range", http.StatusRequestedRangeNotSatisfiable)
				return
			}
			writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
			writer.WriteHeader(http.StatusPartialContent)
			_, _ = writer.Write(content[start : end+1])
		}))
		return server
	}

	allowed := build()
	defer allowed.Close()
	reader, err := Open(t.Context(), fmt.Sprintf("%s/hop/%d", allowed.URL, 5))
	if err != nil {
		t.Fatalf("five redirects must be followed: %v", err)
	}
	reader.Close()

	rejected := build()
	defer rejected.Close()
	if _, err := Open(t.Context(), fmt.Sprintf("%s/hop/%d", rejected.URL, 6)); !errors.Is(err, errRedirectRejected) {
		t.Fatalf("six redirects must be rejected, err = %v", err)
	}
}
