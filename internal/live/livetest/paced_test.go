package livetest

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/testmedia"
)

func drain(t *testing.T, reader io.Reader, limit time.Duration) int {
	t.Helper()
	buffer := make([]byte, 64*packetSize)
	deadline := time.Now().Add(limit)
	total := 0
	for time.Now().Before(deadline) {
		count, err := reader.Read(buffer)
		total += count
		if err != nil {
			break
		}
	}
	return total
}

func TestPacedDeliversAtRealTime(t *testing.T) {
	path := testmedia.Live(t, testmedia.LiveH264AAC)
	source, control := FileSource(path, "paced")
	control.Burst = 0
	reader, err := source.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	start := time.Now()
	total := drain(t, reader, 3*time.Second)
	elapsed := time.Since(start)
	if total == 0 {
		t.Fatal("no bytes delivered")
	}
	unpacedSource, unpacedControl := FileSource(path, "unpaced")
	unpacedControl.Unpaced = true
	unpacedControl.Loop = false
	fast, err := unpacedSource.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer fast.Close()
	all, _ := io.Copy(io.Discard, fast)
	rate := float64(total) / elapsed.Seconds()
	fullRate := float64(all) / 12.0
	if rate < fullRate*0.5 || rate > fullRate*1.6 {
		t.Fatalf("delivered %.0f B/s, file rate about %.0f B/s", rate, fullRate)
	}
	if elapsed < 2500*time.Millisecond {
		t.Fatalf("returned after %v, expected pacing to hold it near 3s", elapsed)
	}
}

func TestPacedLoops(t *testing.T) {
	path := testmedia.Live(t, testmedia.LiveH264AAC)
	source, control := FileSource(path, "loop")
	control.Unpaced = true
	reader, err := source.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	probe, probeControl := FileSource(path, "size")
	probeControl.Unpaced = true
	probeControl.Loop = false
	single, err := probe.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer single.Close()
	size, _ := io.Copy(io.Discard, single)
	buffer := make([]byte, 64*packetSize)
	var total int64
	for total < size*2+int64(len(buffer)) {
		count, err := reader.Read(buffer)
		if err != nil {
			t.Fatalf("read after %d of %d bytes: %v", total, size, err)
		}
		total += int64(count)
	}
	if control.Opens() != 1 {
		t.Fatalf("opens %d", control.Opens())
	}
}

func TestInjectedFailureAndRefusal(t *testing.T) {
	path := testmedia.Live(t, testmedia.LiveH264AAC)
	source, control := FileSource(path, "faults")
	control.RefuseOpens(1)
	if _, err := source.Open(context.Background()); err != ErrInjected {
		t.Fatalf("refusal returned %v", err)
	}
	reader, err := source.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	control.Fail()
	if _, err := reader.Read(make([]byte, 4*packetSize)); err != ErrInjected {
		t.Fatalf("read returned %v", err)
	}
	if control.Opens() != 2 {
		t.Fatalf("opens %d", control.Opens())
	}
}
