package mediasource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/eleven-am/goshl/internal/localpath"
)

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
	if path, ok := localpath.Resolve(sourceURL); ok {
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("%w: open local source: %v", ErrUnsupported, err)
		}
		info, err := file.Stat()
		if err != nil {
			file.Close()
			return nil, fmt.Errorf("stat local source: %w", err)
		}
		return &fileReader{File: file, size: info.Size()}, nil
	}

	parsed, err := url.Parse(sourceURL)
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, ErrUnsupported
	}
	reader := &httpReader{ctx: ctx, client: http.DefaultClient, url: sourceURL}
	size, err := reader.probeSize()
	if err != nil {
		return nil, err
	}
	reader.size = size
	return reader, nil
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
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("%w: range request returned status %d", ErrUnsupported, resp.StatusCode)
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
