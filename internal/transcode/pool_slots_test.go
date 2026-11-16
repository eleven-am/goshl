package transcode

import (
	"context"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/encoderslots"
	"github.com/eleven-am/goshl/internal/ffmpeg"
)

func slotPool(stream domain.StreamType, slots *encoderslots.Budget) (*Pool, *stubCoordinator) {
	coordinator := &stubCoordinator{}
	builder := ffmpeg.NewCommandBuilder(&domain.HWAccelConfig{
		Accelerator:  domain.AccelNone,
		EncodeFlags:  []string{"-c:v", "libx264", "-preset", "veryfast"},
		KeyframeFlag: "-force_key_frames",
		ScaleFilter:  "scale=%d:%d,setsar=1,format=yuv420p",
	})
	storage := &metadataStorage{}
	return NewPool(coordinator, 1, stream, storage, builder, storage, PoolConfig{Profile: "profile", JobTimeout: time.Minute, Slots: slots}), coordinator
}

func slotJob(kind string) (domain.StreamType, domain.Job) {
	switch kind {
	case "video-transcode":
		return domain.StreamVideo, contractJob(domain.StreamVideo)
	case "video-copy":
		return domain.StreamVideo, copiedContractJob()
	case "audio":
		return domain.StreamAudio, contractJob(domain.StreamAudio)
	default:
		return domain.StreamChannel, channelContractJob()
	}
}

func runSlotJob(t *testing.T, kind string, slots *encoderslots.Budget) (<-chan struct{}, *stubCoordinator) {
	t.Helper()
	stream, job := slotJob(kind)
	pool, coordinator := slotPool(stream, slots)
	done := make(chan struct{})
	go func() {
		defer close(done)
		pool.processJob(context.Background(), domain.JobDelivery{Job: sealed(job), Receipt: "receipt"})
	}()
	return done, coordinator
}

func TestVideoTranscodeJobWaitsForAnEncoderSlot(t *testing.T) {
	slots := encoderslots.New(1, 0)
	if err := slots.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	done, _ := runSlotJob(t, "video-transcode", slots)
	select {
	case <-done:
		t.Fatal("a video transcode job ran while every encoder slot was held")
	case <-time.After(400 * time.Millisecond):
	}
	slots.Release()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the job did not proceed once a slot was free")
	}
}

func TestJobsWithoutAnEncoderDoNotTakeASlot(t *testing.T) {
	for _, kind := range []string{"video-copy", "audio", "channel"} {
		slots := encoderslots.New(1, 0)
		if err := slots.Acquire(context.Background()); err != nil {
			t.Fatal(err)
		}
		done, _ := runSlotJob(t, kind, slots)
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Errorf("%s job waited for an encoder slot", kind)
		}
	}
}

func TestVideoTranscodeJobReleasesItsSlotWhenItFails(t *testing.T) {
	slots := encoderslots.New(1, 0)
	done, coordinator := runSlotJob(t, "video-transcode", slots)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the job did not finish")
	}
	if len(coordinator.jobPublishes) == 0 {
		t.Fatal("the job did not settle")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := slots.Acquire(ctx); err != nil {
		t.Fatalf("the slot was not released: %v", err)
	}
}

func TestVideoTranscodeJobRunsWithoutABudget(t *testing.T) {
	done, _ := runSlotJob(t, "video-transcode", nil)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("a pool without a budget must not wait")
	}
}
