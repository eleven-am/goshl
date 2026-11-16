package transcode

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
)

func channelContractSegment(index int) domain.Segment {
	start := domain.ChannelFrameTime(index*48, 24000, 1001)
	end := domain.ChannelFrameTime((index+1)*48, 24000, 1001)
	return domain.Segment{Index: index, Start: start, End: end, Duration: end - start}
}

func channelContractJob() domain.Job {
	overlap, trailing := channelContractSegment(9), channelContractSegment(12)
	job := contractJob(domain.StreamChannel)
	job.Rendition = "channel-720p-a0-1a18aabd6d89"
	job.StartIndex, job.EndIndex, job.RequestedIndex = 10, 11, 11
	job.Segments = []domain.Segment{channelContractSegment(10), channelContractSegment(11)}
	job.ExpectedIndices = []int{10, 11}
	job.Overlap, job.Trailing = &overlap, &trailing
	job.Priority = domain.PriorityChannel
	job.VideoStreamIndex = 0
	job.VideoRendition = &domain.VideoRendition{Name: job.Rendition, Method: domain.Transcode, Width: 1280, Height: 720, Bitrate: 2_500_000, PeakBitrate: 2_500_000, BufferSize: 5_000_000, Level: 31}
	job.AudioRendition = &domain.AudioRendition{Name: job.Rendition, Codec: "aac", Method: domain.Transcode, Channels: 2, Bitrate: 160_000, SampleRate: 48000, StreamIndex: 1}
	job.Channel = &domain.ChannelSpec{FormatVersion: domain.ChannelFormatVersion, Width: 1280, Height: 720, PictureWidth: 1280, PictureHeight: 720, FrameRateNum: 24000, FrameRateDen: 1001, SegmentFrames: 48, SegmentsPerJob: 2, TotalFrames: 4800}
	return job
}

func TestValidateJobAcceptsAChannelJob(t *testing.T) {
	if err := contractPool(domain.StreamChannel).validateJob(sealed(channelContractJob())); err != nil {
		t.Fatal(err)
	}
}

func TestValidateJobRejectsInconsistentChannelJobs(t *testing.T) {
	cases := []struct {
		name  string
		spoil func(*domain.Job)
		want  string
	}{
		{"no channel parameters", func(j *domain.Job) { j.Channel = nil }, "channel parameters are missing"},
		{"no audio rendition", func(j *domain.Job) { j.AudioRendition = nil }, "channel parameters are missing"},
		{"no video rendition", func(j *domain.Job) { j.VideoRendition = nil }, "channel parameters are missing"},
		{"audio rendition of another name", func(j *domain.Job) { j.AudioRendition.Name = "aac_stereo" }, "rendition identity"},
		{"another format version", func(j *domain.Job) { j.Channel.FormatVersion++ }, "format version"},
		{"batch off the grid", func(j *domain.Job) { j.Channel.SegmentsPerJob = 3 }, "off the grid"},
		{"batch longer than the grid", func(j *domain.Job) { j.Channel.SegmentsPerJob = 1 }, "off the grid"},
		{"copied video", func(j *domain.Job) { j.VideoRendition.Method = domain.DirectStream }, "must be transcoded"},
		{"no overlap", func(j *domain.Job) { j.Overlap = nil }, "overlap is missing"},
		{"detached trailing", func(j *domain.Job) { j.Trailing.Start += 2 }, "trailing is inconsistent"},
		{"video stream", func(j *domain.Job) { j.StreamType = domain.StreamVideo }, "stream or segment range"},
	}
	for _, c := range cases {
		job := channelContractJob()
		c.spoil(&job)
		if err := contractPool(domain.StreamChannel).validateJob(sealed(job)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", c.name, err, c.want)
		}
	}
	lowered := sealed(channelContractJob())
	lowered.Priority = 0
	if err := contractPool(domain.StreamChannel).validateJob(lowered); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Errorf("a coordinator that drops Priority must be caught by the digest, got %v", err)
	}
	video := contractJob(domain.StreamVideo)
	video.Channel = channelContractJob().Channel
	if err := contractPool(domain.StreamVideo).validateJob(sealed(video)); err == nil || !strings.Contains(err.Error(), "channel parameters on a video job") {
		t.Errorf("a video job must not carry channel parameters, got %v", err)
	}
}

func TestChannelParamsCarryTheSourceFrameRateOfTheJob(t *testing.T) {
	jobIn := channelContractJob()
	jobIn.Channel.SourceFrameRate = 25
	job := sealed(jobIn)
	params := contractPool(domain.StreamChannel).channelParams(job, job.Segments, "out")
	if params.Video.FrameRate != 25 {
		t.Errorf("Video.FrameRate = %v, want the source rate 25", params.Video.FrameRate)
	}
	if params.InputURL != job.SourceURL || params.OutputDir != "out" || params.Spec.SegmentFrames != 48 || params.Video.Width != 1280 {
		t.Errorf("the rest of the job was not carried: %+v", params)
	}
	if job.VideoRendition.FrameRate != 0 {
		t.Errorf("the job rendition was mutated: %v", job.VideoRendition.FrameRate)
	}
}

func TestChannelParamsNeverReadStorage(t *testing.T) {
	storage := &metadataStorage{err: errors.New("storage must not be read")}
	pool := NewPool(&stubCoordinator{}, 1, domain.StreamChannel, storage, nil, nil, PoolConfig{Profile: "profile"})
	job := sealed(channelContractJob())
	pool.channelParams(job, job.Segments, "out")
	if len(storage.keys) != 0 {
		t.Errorf("channelParams read metadata for %v", storage.keys)
	}
}

func TestChannelJobDigestCoversTheSourceFrameRate(t *testing.T) {
	base := channelContractJob()
	base.Channel.SourceFrameRate = 25
	stripped := sealed(base)
	stripped.Channel = &domain.ChannelSpec{}
	*stripped.Channel = *base.Channel
	stripped.Channel.SourceFrameRate = 0
	if err := contractPool(domain.StreamChannel).validateJob(stripped); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Errorf("a coordinator that drops the source frame rate must be caught by the digest, got %v", err)
	}
	if err := contractPool(domain.StreamChannel).validateJob(sealed(base)); err != nil {
		t.Errorf("a job with a source frame rate must be accepted: %v", err)
	}
}

func TestValidateJobRejectsAnUnusableChannelSourceFrameRate(t *testing.T) {
	for _, rate := range []float64{-1, math.NaN(), math.Inf(1)} {
		job := channelContractJob()
		job.Channel.SourceFrameRate = rate
		if err := contractPool(domain.StreamChannel).validateJob(job); err == nil || !strings.Contains(err.Error(), "source frame rate") {
			t.Errorf("rate %v: got %v", rate, err)
		}
	}
}

type metadataStorage struct {
	memoryStorage
	data []byte
	err  error
	keys []string
}

func (m *metadataStorage) GetMetadata(ctx context.Context, mediaKey string) ([]byte, error) {
	m.keys = append(m.keys, mediaKey)
	return m.data, m.err
}
