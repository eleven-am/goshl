package transcode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/ffmpeg"
)

func contractSegment(index int) domain.Segment {
	return domain.Segment{Index: index, Start: float64(index) * 6, End: float64(index+1) * 6, Duration: 6}
}

func contractJob(stream domain.StreamType) domain.Job {
	overlap := contractSegment(0)
	job := domain.Job{
		ID:              "job",
		RunID:           "run",
		MediaKey:        "media",
		Profile:         "profile",
		SourceURL:       "source",
		Rendition:       "r",
		StreamType:      stream,
		StartIndex:      1,
		EndIndex:        2,
		RequestedIndex:  1,
		EnqueuedAt:      time.Now(),
		StartBy:         time.Now().Add(time.Minute),
		Segments:        []domain.Segment{contractSegment(1), contractSegment(2)},
		ExpectedIndices: []int{1, 2},
	}
	if stream == domain.StreamVideo {
		job.VideoRendition = &domain.VideoRendition{Name: "r", Method: domain.Transcode, Width: 1280, Height: 720, Bitrate: 2_000_000, PeakBitrate: 3_000_000, BufferSize: 6_000_000, Level: 31}
		job.VideoStreamIndex = 2
		return job
	}
	job.Overlap = &overlap
	job.AudioRendition = &domain.AudioRendition{Name: "r", Codec: "aac", Method: domain.Transcode, Channels: 2, Bitrate: 128000, SampleRate: 44100, StreamIndex: 1, SourceEnd: 30}
	return job
}

func copiedContractJob() domain.Job {
	overlap, trailing := contractSegment(0), contractSegment(3)
	job := contractJob(domain.StreamVideo)
	job.VideoRendition = &domain.VideoRendition{Name: "r", Method: domain.DirectStream}
	job.Overlap, job.Trailing = &overlap, &trailing
	return job
}

func contractPool(stream domain.StreamType) *Pool {
	return NewPool(&stubCoordinator{}, 1, stream, nil, nil, nil, PoolConfig{Profile: "profile"})
}

func TestValidateJobAcceptsWhatTheControllerBuilds(t *testing.T) {
	for name, job := range map[string]domain.Job{"transcoded video": contractJob(domain.StreamVideo), "copied video": copiedContractJob(), "audio": contractJob(domain.StreamAudio)} {
		if err := contractPool(job.StreamType).validateJob(sealed(job)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestValidateJobRejectsEveryInconsistentField(t *testing.T) {
	video, audio := domain.StreamVideo, domain.StreamAudio
	cases := []struct {
		name   string
		stream domain.StreamType
		spoil  func(*domain.Job)
		want   string
	}{
		{"empty ID", video, func(j *domain.Job) { j.ID = "" }, "required identity"},
		{"empty RunID", video, func(j *domain.Job) { j.RunID = "" }, "required identity"},
		{"empty MediaKey", video, func(j *domain.Job) { j.MediaKey = "" }, "required identity"},
		{"empty Profile", video, func(j *domain.Job) { j.Profile = "" }, "required identity"},
		{"empty SourceURL", video, func(j *domain.Job) { j.SourceURL = "" }, "required identity"},
		{"empty Rendition", video, func(j *domain.Job) { j.Rendition = "" }, "required identity"},
		{"profile of another pipeline", video, func(j *domain.Job) { j.Profile = "other" }, "profile mismatch"},
		{"zero EnqueuedAt", video, func(j *domain.Job) { j.EnqueuedAt = time.Time{} }, "required identity"},
		{"zero StartBy", video, func(j *domain.Job) { j.StartBy = time.Time{} }, "required identity"},
		{"audio job on the video pool", video, func(j *domain.Job) { j.StreamType = audio }, "stream or segment range"},
		{"StartIndex off", video, func(j *domain.Job) { j.StartIndex = 0; j.RequestedIndex = 1 }, "indexes are inconsistent"},
		{"EndIndex off", video, func(j *domain.Job) { j.EndIndex = 3 }, "indexes are inconsistent"},
		{"RequestedIndex below the range", video, func(j *domain.Job) { j.RequestedIndex = 0 }, "indexes are inconsistent"},
		{"RequestedIndex above the range", video, func(j *domain.Job) { j.RequestedIndex = 3 }, "indexes are inconsistent"},
		{"segment index gap", video, func(j *domain.Job) {
			j.Segments[1] = contractSegment(3)
			j.EndIndex, j.ExpectedIndices = 3, []int{1, 3}
		}, "segment 3 is inconsistent"},
		{"expected index differs", video, func(j *domain.Job) { j.ExpectedIndices = []int{1, 7} }, "segment 2 is inconsistent"},
		{"negative start", video, func(j *domain.Job) { j.Segments[0] = domain.Segment{Index: 1, Start: -1, End: 6, Duration: 7} }, "segment 1 is inconsistent"},
		{"duration disagrees with the bounds", video, func(j *domain.Job) { j.Segments[1].Duration = 6.001 }, "segment 2 is inconsistent"},
		{"video rendition missing", video, func(j *domain.Job) { j.VideoRendition = nil }, "rendition parameters are missing"},
		{"audio rendition missing", audio, func(j *domain.Job) { j.AudioRendition = nil }, "rendition parameters are missing"},
		{"video rendition name differs", video, func(j *domain.Job) { j.VideoRendition.Name = "other" }, "rendition identity"},
		{"audio rendition name differs", audio, func(j *domain.Job) { j.AudioRendition.Name = "other" }, "rendition identity"},
		{"video method dropped", video, func(j *domain.Job) { j.VideoRendition.Method = "" }, "video method"},
		{"audio method dropped", audio, func(j *domain.Job) { j.AudioRendition.Method = "" }, "audio method"},
		{"audio channels dropped", audio, func(j *domain.Job) { j.AudioRendition.Channels = 0 }, "channels or bitrate"},
		{"audio bitrate dropped", audio, func(j *domain.Job) { j.AudioRendition.Bitrate = 0 }, "channels or bitrate"},
		{"passthrough codec dropped", audio, func(j *domain.Job) {
			j.AudioRendition.Method, j.AudioRendition.Codec = domain.DirectStream, ""
		}, "audio codec"},
		{"audio stream index disagrees with the track name", audio, func(j *domain.Job) {
			j.Rendition, j.AudioRendition.Name, j.AudioRendition.StreamIndex = "audio3_aac_stereo", "audio3_aac_stereo", 0
		}, "audio stream index"},
		{"audio overlap dropped", audio, func(j *domain.Job) { j.Overlap = nil }, "overlap is missing"},
		{"digest missing", video, func(j *domain.Job) {}, "digest mismatch"},
	}
	for _, test := range cases {
		job := contractJob(test.stream)
		job.Segments = append([]domain.Segment(nil), job.Segments...)
		test.spoil(&job)
		if test.name != "digest missing" {
			job = sealed(job)
		}
		err := contractPool(test.stream).validateJob(job)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: got %v, want an error containing %q", test.name, err, test.want)
		}
	}
	copied := copiedContractJob()
	copied.Overlap = nil
	if err := contractPool(video).validateJob(sealed(copied)); err == nil || !strings.Contains(err.Error(), "overlap is missing") {
		t.Errorf("copied video without its overlap: got %v", err)
	}
}

func TestValidateJobDetectsFieldsACoordinatorDropped(t *testing.T) {
	video, audio := domain.StreamVideo, domain.StreamAudio
	hdr := func() domain.Job {
		job := contractJob(video)
		job.VideoRendition.Colour = domain.ColourPlan{Tonemap: true, AssumeMatrix: "bt2020nc", AssumePrimaries: "bt2020"}
		job.VideoRendition.SourceEnd = 17.5
		return job
	}
	cases := []struct {
		name string
		job  domain.Job
		drop func(*domain.Job)
	}{
		{"VideoStreamIndex", contractJob(video), func(j *domain.Job) { j.VideoStreamIndex = 0 }},
		{"Colour.Tonemap", hdr(), func(j *domain.Job) { j.VideoRendition.Colour.Tonemap = false }},
		{"Colour.AssumeMatrix", hdr(), func(j *domain.Job) { j.VideoRendition.Colour.AssumeMatrix = "" }},
		{"Colour.AssumePrimaries", hdr(), func(j *domain.Job) { j.VideoRendition.Colour.AssumePrimaries = "" }},
		{"video SourceEnd", hdr(), func(j *domain.Job) { j.VideoRendition.SourceEnd = 0 }},
		{"BufferSize", contractJob(video), func(j *domain.Job) { j.VideoRendition.BufferSize = 0 }},
		{"Trailing", copiedContractJob(), func(j *domain.Job) { j.Trailing = nil }},
		{"audio StreamIndex", contractJob(audio), func(j *domain.Job) { j.AudioRendition.StreamIndex = 0 }},
		{"SampleRate", contractJob(audio), func(j *domain.Job) { j.AudioRendition.SampleRate = 0 }},
		{"audio SourceEnd", contractJob(audio), func(j *domain.Job) { j.AudioRendition.SourceEnd = 0 }},
		{"rounded segment boundary", contractJob(audio), func(j *domain.Job) {
			j.Segments = []domain.Segment{j.Segments[0], {Index: 2, Start: 12, End: 18.0000004, Duration: 6.0000004}}
		}},
	}
	for _, test := range cases {
		job := sealed(test.job)
		if test.job.VideoRendition != nil {
			rendition := *test.job.VideoRendition
			job.VideoRendition = &rendition
		}
		if test.job.AudioRendition != nil {
			rendition := *test.job.AudioRendition
			job.AudioRendition = &rendition
		}
		test.drop(&job)
		err := contractPool(job.StreamType).validateJob(job)
		if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
			t.Errorf("dropping %s: got %v, want a digest mismatch", test.name, err)
		}
	}
}

func TestValidateJobAcceptsAJobCarriedAsJSON(t *testing.T) {
	for _, job := range []domain.Job{contractJob(domain.StreamVideo), copiedContractJob(), contractJob(domain.StreamAudio)} {
		data, err := json.Marshal(sealed(job))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var carried domain.Job
		if err := json.Unmarshal(data, &carried); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		carried.EnqueuedAt = carried.EnqueuedAt.Truncate(time.Second).Local()
		carried.StartBy = carried.StartBy.Truncate(time.Millisecond).UTC()
		if err := contractPool(job.StreamType).validateJob(carried); err != nil {
			t.Errorf("%s job carried as JSON must stay valid: %v", job.StreamType, err)
		}
	}
}

func TestPoolRejectsDeliveryWithAlteredJob(t *testing.T) {
	coord := &stubCoordinator{}
	recorder := newEventRecorder()
	pool := NewPool(coord, 1, domain.StreamVideo, nil, nil, nil, PoolConfig{Profile: "profile", Observer: recorder})
	job := sealed(contractJob(domain.StreamVideo))
	job.VideoStreamIndex = 0

	pool.processJob(context.Background(), domain.JobDelivery{Job: job, Receipt: "receipt"})

	if recorder.count(domain.EventJobDeliveryRejected) != 1 || recorder.count(domain.EventJobStarted) != 0 {
		t.Fatalf("an altered job must be rejected before it starts: %v", recorder.snapshot())
	}
	if len(coord.jobPublishes) != 1 || coord.jobPublishes[0].State != domain.JobStateFailed || !strings.Contains(coord.jobPublishes[0].Error, "digest mismatch") {
		t.Fatalf("an altered job must settle failed with the digest error: %v", coord.jobPublishes)
	}
}

func TestSegmentEndsFollowThePlanAndItsTolerances(t *testing.T) {
	offset := domain.OutputTimelineOffset.Seconds()
	audio := contractJob(domain.StreamAudio)
	got := segmentEnds(audio, false)
	want := map[int]SegmentEnd{
		0: {Time: offset + 6, Early: 0.06, Late: 0.06},
		1: {Time: offset + 12, Early: 0.06, Late: 0.06},
		2: {Time: offset + 18, Early: domain.StreamTailSlack, Late: 0.06},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("audio ends = %v, want %v", got, want)
	}
	copied := copiedContractJob()
	got = segmentEnds(copied, true)
	want = map[int]SegmentEnd{
		0: {Time: offset + 6, Early: 0.1, Late: 0.1},
		1: {Time: offset + 12, Early: 0.1, Late: 0.1},
		2: {Time: offset + 18, Early: 0.1, Late: 0.1},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("copied video ends = %v, want %v", got, want)
	}
}

func TestWorkerEndCheckBounds(t *testing.T) {
	cases := []struct {
		name   string
		line   string
		end    SegmentEnd
		reject bool
	}{
		{"early end", "segment-00001.ts,0.000000,14.800000", SegmentEnd{Time: 15, Early: 0.1, Late: 0.1}, true},
		{"unreadable end", "segment-00001.ts,0.000000,later", SegmentEnd{Time: 15, Early: 0.1, Late: 0.1}, true},
		{"end exactly on the early bound", "segment-00001.ts,0.000000,14.500000", SegmentEnd{Time: 15, Early: 0.5, Late: 0.1}, false},
	}
	for _, test := range cases {
		w, storage := runWorkerWithLines(t, []string{test.line}, map[int]SegmentEnd{1: test.end}, []int{1})
		if rejected := errors.Is(w.Err(), ErrSegmentTimeline); rejected != test.reject {
			t.Errorf("%s: worker error = %v, writes %v", test.name, w.Err(), storage.writes)
		}
	}
}

func TestWorkerChecksTheEndOfADroppedNeighbour(t *testing.T) {
	tmp := installFakeFFmpeg(t, csvFFmpegScript)
	storage := &memoryStorage{}
	ends := map[int]SegmentEnd{0: {Time: 16, Early: 0.06, Late: 0.06}, 1: {Time: 22, Early: 0.06, Late: 0.06}}
	w := NewWorker(WorkerSpec{Args: []string{"--emit", "segment-00000.ts,10.000000,16.400000", "segment-00001.ts,16.400000,22.000000"}, Storage: storage, MediaKey: "media", Rendition: "r", TmpDir: tmp, Expected: []int{1}, Overlap: segmentNumber(0), Ends: ends})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitWorker(t, w, 10*time.Second)
	if !errors.Is(w.Err(), ErrSegmentTimeline) || len(storage.writes) != 0 {
		t.Fatalf("an overlap segment that ends off the plan must fail the job: err %v, writes %v", w.Err(), storage.writes)
	}
}

const planEndFFmpegScript = `#!/bin/sh
for last; do :; done
dir=$(dirname "$last")
printf data > "$dir/segment-00000.ts"
printf data > "$dir/segment-00001.ts"
echo "segment-00000.ts,10.000000,$GOSHL_TEST_FIRST_END"
echo "segment-00001.ts,$GOSHL_TEST_FIRST_END,22.000000"
`

func TestPoolFailsJobWhoseOutputLeavesThePlan(t *testing.T) {
	for end, wantState := range map[string]domain.JobState{"16.020000": domain.JobStateCompleted, "16.400000": domain.JobStateFailed, "15.600000": domain.JobStateFailed} {
		installFakeFFmpeg(t, planEndFFmpegScript)
		t.Setenv("GOSHL_TEST_FIRST_END", end)
		storage := newCapturingStorage()
		coord := &stubCoordinator{}
		builder := ffmpeg.NewCommandBuilder(&domain.HWAccelConfig{Accelerator: domain.AccelNone})
		pool := NewPool(coord, 1, domain.StreamAudio, storage, builder, storage, PoolConfig{Profile: "profile"})
		job := contractJob(domain.StreamAudio)
		job.Overlap = nil
		job.StartIndex, job.EndIndex, job.RequestedIndex = 0, 1, 0
		job.Segments, job.ExpectedIndices = []domain.Segment{contractSegment(0), contractSegment(1)}, []int{0, 1}

		pool.processJob(context.Background(), domain.JobDelivery{Job: sealed(job), Receipt: "receipt"})

		if len(coord.jobPublishes) != 1 || coord.jobPublishes[0].State != wantState {
			t.Fatalf("first segment ending at %s: statuses %v, want %s", end, coord.jobPublishes, wantState)
		}
		if wantState == domain.JobStateFailed && (!strings.Contains(coord.jobPublishes[0].Error, ErrSegmentTimeline.Error()) || len(storage.writes) != 0) {
			t.Fatalf("first segment ending at %s: error %q, stored %v", end, coord.jobPublishes[0].Error, storage.writes)
		}
	}
}
