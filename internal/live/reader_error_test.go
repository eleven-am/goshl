package live_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/live"
)

var errReaderBroke = errors.New("reader broke mid-stream")

type breakingReader struct {
	inner io.ReadCloser
	limit int
	read  int
}

func (b *breakingReader) Read(buffer []byte) (int, error) {
	if b.read > b.limit {
		return 0, errReaderBroke
	}
	n, err := b.inner.Read(buffer)
	b.read += n
	return n, err
}

func (b *breakingReader) Close() error { return b.inner.Close() }

func TestReaderErrorDuringEpochStaysInspectable(t *testing.T) {
	r := newRig(t, func(config *live.Config) {
		config.ReconnectAttempts = -1
	})
	inner, _ := source(t, "h264_aac")
	src := live.Source{Label: "breaking", Open: func(ctx context.Context) (io.ReadCloser, error) {
		reader, err := inner.Open(ctx)
		if err != nil {
			return nil, err
		}
		return &breakingReader{inner: reader, limit: 3 << 20}, nil
	}}
	session, err := r.manager.Live(context.Background(), "breaking-key", src, fastOptions())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.Done():
	case <-time.After(40 * time.Second):
		t.Fatal("session did not fail")
	}
	state := session.State()
	if !errors.Is(state.Err, live.ErrSourceUnavailable) {
		t.Fatalf("error %v is not ErrSourceUnavailable", state.Err)
	}
	if !errors.Is(state.Err, errReaderBroke) {
		t.Fatalf("error %v does not wrap the reader's own error", state.Err)
	}
}
