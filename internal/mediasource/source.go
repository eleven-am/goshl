package mediasource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eleven-am/goshl/internal/source"
)

const maxRedirects = 5

var errRedirectRejected = errors.New("redirect rejected")

var ErrUnsupported = errors.New("media source does not support random access")

type Reader interface {
	io.ReaderAt
	Size() int64
	Close() error
}

type fileReader struct {
	*os.File
	size int64
}

func (r *fileReader) Size() int64 {
	return r.size
}

type httpReader struct {
	ctx    context.Context
	client *http.Client
	url    string
	size   int64
}

func Open(ctx context.Context, sourceURL string) (Reader, error) {
	input, err := source.Resolve(sourceURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	if !input.Remote {
		file, err := os.Open(input.LocalPath)
		if err != nil {
			return nil, err
		}
		info, err := file.Stat()
		if err != nil {
			file.Close()
			return nil, fmt.Errorf("stat local source: %w", err)
		}
		return &fileReader{File: file, size: info.Size()}, nil
	}

	reader := &httpReader{ctx: ctx, client: sharedRemoteClient(), url: input.Arg}
	size, err := reader.probeSize()
	if err != nil {
		return nil, err
	}
	reader.size = size
	return reader, nil
}

var (
	remoteClientOnce sync.Once
	remoteClient     *http.Client
)

func sharedRemoteClient() *http.Client {
	remoteClientOnce.Do(func() {
		remoteClient = newRemoteClient()
	})
	return remoteClient
}

func newRemoteClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 10 * time.Second
	transport.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{Transport: transport, CheckRedirect: checkRedirect}
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > maxRedirects {
		return fmt.Errorf("%w: stopped after %d redirects", errRedirectRejected, maxRedirects)
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("%w: unsupported scheme %q", errRedirectRejected, req.URL.Scheme)
	}
	if previous := via[len(via)-1]; previous.URL.Scheme == "https" && req.URL.Scheme == "http" {
		return fmt.Errorf("%w: https to http downgrade", errRedirectRejected)
	}
	return nil
}

func (r *httpReader) Size() int64 {
	return r.size
}

func (r *httpReader) Close() error {
	return nil
}

func (r *httpReader) probeSize() (int64, error) {
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return 0, fmt.Errorf("create range probe: %w", err)
	}
	req.Header.Set("Range", "bytes=0-0")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("probe range source: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("%w: range probe returned status %d", ErrUnsupported, resp.StatusCode)
	}
	start, end, total, err := parseContentRange(resp.Header.Get("Content-Range"))
	if err != nil || start != 0 || end != 0 || total <= 0 {
		return 0, fmt.Errorf("%w: invalid range probe response", ErrUnsupported)
	}
	var one [1]byte
	if _, err := io.ReadFull(resp.Body, one[:]); err != nil {
		return 0, fmt.Errorf("read range probe: %w", err)
	}
	return total, nil
}

func (r *httpReader) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 {
		return 0, fmt.Errorf("negative source offset %d", off)
	}
	if off >= r.size {
		return 0, io.EOF
	}
	want := len(p)
	if remaining := r.size - off; int64(want) > remaining {
		want = int(remaining)
	}
	end := off + int64(want) - 1
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return 0, fmt.Errorf("create range request: %w", err)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, end))
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("read source range: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return 0, fmt.Errorf("%w: range request returned the whole resource", ErrUnsupported)
	}
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("read source range: unexpected status %d", resp.StatusCode)
	}
	start, responseEnd, total, err := parseContentRange(resp.Header.Get("Content-Range"))
	if err != nil || start != off || responseEnd != end || total != r.size {
		return 0, fmt.Errorf("invalid range response")
	}
	n, err := io.ReadFull(resp.Body, p[:want])
	if err != nil {
		return n, fmt.Errorf("read source range: %w", err)
	}
	if want < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func parseContentRange(value string) (int64, int64, int64, error) {
	if !strings.HasPrefix(value, "bytes ") {
		return 0, 0, 0, errors.New("missing bytes unit")
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes "), "/")
	if len(parts) != 2 {
		return 0, 0, 0, errors.New("invalid content range")
	}
	bounds := strings.Split(parts[0], "-")
	if len(bounds) != 2 {
		return 0, 0, 0, errors.New("invalid content range bounds")
	}
	start, err := strconv.ParseInt(bounds[0], 10, 64)
	if err != nil {
		return 0, 0, 0, err
	}
	end, err := strconv.ParseInt(bounds[1], 10, 64)
	if err != nil {
		return 0, 0, 0, err
	}
	total, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, 0, err
	}
	if start < 0 || end < start || total <= end {
		return 0, 0, 0, errors.New("content range is out of bounds")
	}
	return start, end, total, nil
}
