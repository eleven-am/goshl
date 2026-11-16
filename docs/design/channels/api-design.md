# goshl channel API: design

Status: design only, against goshl v0.0.28 (cache `goshl-cache-v6`, job contract 5). Nothing in the repo was changed.

Assumption: the channel output format (fixed canvas up to 1280x720, CFR, muxed A/V MPEG-TS, fixed-length segments, one audio track per item) exists as a new ffmpeg command, `CommandBuilder.Channel`, owned by the format prototype. This document covers the API, planning, flights, priority and contract around it.

## 1. Decisions at a glance

| # | Question | Decision |
|---|---|---|
| D1 | Profile per Controller or per call | Per call. `ChannelProfile` is a value passed to every channel method. Only the worker lane size is per Controller (`Options.ChannelPoolSize`). |
| D2 | Identity | Same media key as VOD. A reserved rendition name `channel-...` that hashes the normalised profile, the audio track and a channel format version. |
| D3 | Planning | New `ChannelPlan`. Not `VariantPlaylist`. Fixed frame grid from the probed duration. |
| D4 | Fetch | `ChannelSegment`, the same flight path as `Segment`. |
| D5 | Prefetch | `ChannelPrefetch`, the `segmentOnce` launch sequence without the wait. Grid-aligned batches. goshl keeps no playhead state. |
| D6 | Isolation | A dedicated worker lane: new `StreamChannel` stream type and a third pool. This is the hard guarantee. |
| D7 | Priority | `Job.Priority` in the digest, job contract 6. Soft ordering inside a lane. |
| D8 | Cache impact of contract 6 | Split the routing profile from the cache identity so the VOD cache stays warm. Needs Roy's approval (alternative: cold cache). |
| D9 | Readiness | No new API. `ChannelPlan` returns the media key and rendition frames needs for its own `SegmentExists`. |
| D10 | Live sources | Separate subsystem, later phase. Sketch only. |

## 2. Public API

### 2.1 Types

```go
type ChannelProfile struct {
	Height          int
	FPS             int
	VideoBitrate    int
	AudioBitrate    int
	SegmentDuration float64
}

type ChannelPlan struct {
	MediaKey  string
	Rendition string
	Duration  float64
	Segments  []Segment
}

func (p ChannelPlan) SegmentData(index int) SegmentData

type JobPriority int

const (
	PriorityBackground JobPriority = 0
	PriorityPlayback   JobPriority = 100
	PriorityChannel    JobPriority = 200
)

const StreamChannel StreamType = "channel"

var (
	ErrInvalidChannelProfile error
	ErrChannelDisabled       error
)
```

`ChannelProfile` fields, defaults and bounds. A zero field takes the default. Identity is computed after defaults are applied, so `ChannelProfile{}` and the explicit defaults share one cache.

| Field | Default | Bounds | Meaning |
|---|---|---|---|
| `Height` | 720 | 240 to 720, even | Canvas height. Canvas width is `Height*16/9` rounded up to even. Content is scaled to fit and padded. |
| `FPS` | 30 | 1 to 60 | Constant output frame rate. Integer, so every segment holds a whole number of frames. |
| `VideoBitrate` | 3,000,000 | 200,000 to 20,000,000 | Bits per second. |
| `AudioBitrate` | 128,000 | 32,000 to 512,000 | Bits per second, AAC stereo 48 kHz. |
| `SegmentDuration` | 2.0 | 1 to 10 seconds | `FPS * SegmentDuration` must be an integer within 1e-9. |

An out-of-range profile returns an error wrapping `ErrInvalidChannelProfile`.

`audioTrack` is the 0-based position in `Metadata.Audios`, not the container stream index. For a source with no audio it must be 0 and the output carries silence. A position past the end returns `ErrRenditionNotFound`.

`ChannelPlan.SegmentData(i)` returns `SegmentData{MediaKey, Index: i, Rendition, IsVideo: true}`. It does not range-check.

### 2.2 Controller methods

```go
func (c *Controller) ChannelPlan(ctx context.Context, sourceURL string, profile ChannelProfile, audioTrack int) (ChannelPlan, error)

func (c *Controller) ChannelSegment(ctx context.Context, sourceURL string, profile ChannelProfile, audioTrack int, index int) ([]byte, error)

func (c *Controller) ChannelPrefetch(ctx context.Context, sourceURL string, profile ChannelProfile, audioTrack int, from int, count int) error
```

All three return `ErrControllerStopped` first, then `ErrChannelDisabled` when `Options.ChannelPoolSize` is 0, then validate the profile.

### 2.3 Options

| Field | Type | Default | Bounds | Meaning |
|---|---|---|---|---|
| `ChannelPoolSize` | `int` | 0 | 0 to 1024 | Channel workers on this replica. 0 disables the channel API and creates no third subscription, so existing hosts see no change. |
| `ChannelSegmentsPerJob` | `int` | 10 | 1 to 1000 | Segments per channel job (20 s at the default profile). Not part of any identity. |

### 2.4 Why per call (D1)

- The job carries the full profile, so workers need no configuration.
- Identity already separates profiles, so several profiles coexist in one cache.
- A second profile (a 480p variant of a channel, a later ABR ladder) needs no second Controller.
- The only real per-replica resource is worker capacity, and that is what `ChannelPoolSize` sets.

## 3. Identity and caching (D2)

```
mediaKey  = unchanged: hash(schema, sourceURL, revision, pipelineVersion)
rendition = "channel-" + Height + "p" + FPS + "-a" + audioTrack + "-" + hex12(sha256(json{ChannelFormatVersion, normalised profile, audioTrack}))
segment   = SegmentData{MediaKey, Index, Rendition, IsVideo: true}
jobID     = deterministicJobID(mediaKey, rendition, StreamChannel, start, end)
```

Example: `channel-720p30-a1-3fa94c01b2d7` (29 characters, passes `rendition.ValidName`).

- The prefix `channel-` is reserved. VOD names are `<N>p`, `aac_stereo`, `aac_surround`, `<codec>_passthrough` and `audio<N>_...`; none contains a hyphen. `Segment` and `VariantPlaylist` reject names with the prefix (`ErrRenditionNotFound`), so the two index spaces can never be mixed through the VOD API.
- `domain.ChannelFormatVersion` (starts at 1) is bumped when the channel ffmpeg command changes its output. That starts a new namespace for channel segments only. VOD is untouched.
- Sharing the media key means: one probe and one stored metadata per source, `Invalidate` and `DeleteMedia` remove channel segments with everything else, and the accelerator, `CacheVersion` and source revision already separate namespaces.
- Nothing about the channel, the schedule slot or the wall clock is in the identity. A rerun, a second channel airing the same item, or a replica restart all hit the same segments.
- Frames' `Storage` needs no change if it stores `SegmentData` fields opaquely. If it parses `Rendition`, it must accept the new prefix.

Rejected: a separate media key per profile. It would duplicate metadata, probe twice and make `Invalidate(source)` miss channel segments.

## 4. Planning (D3)

`VariantPlaylist` is not reused. It returns a rendered VOD playlist, uses the keyframe planner (variable durations up to 12 s) and needs `PathGenerator` URLs. Frames needs numbers, not text.

### 4.1 Algorithm

New pure function `playlist.CalculateChannelSegments(duration float64, fps int, segmentDuration float64) []domain.Segment`. All arithmetic is on integer frame counts.

```
n = round(fps * segmentDuration)            frames per full segment
T = max(1, round(duration * fps))           frames in the item
full = T / n, rem = T % n
tail = round(0.5 * fps)                     sliver threshold in frames
if rem == 0:                 count = full
else if full > 0 && rem < tail: count = full           (last segment has n + rem frames)
else:                        count = full + 1          (last segment has rem frames)
segment i: Start = i*n / fps, End = (i == count-1 ? T : (i+1)*n) / fps, Duration = End - Start
```

- Segment `i` always starts at `i * SegmentDuration`. Only the last segment depends on the duration.
- Last-segment rule: a remainder of at least 0.5 s is its own short segment. A remainder under 0.5 s merges into the previous segment, the same sliver rule as the VOD planner (`playlist.mergeTail`). The last segment therefore lasts from 0.5 s to under `SegmentDuration + 0.5 s`. For an integer `SegmentDuration` its EXTINF still rounds to at most `SegmentDuration`, so one `EXT-X-TARGETDURATION` holds across items.
- An item shorter than one segment yields one segment of `T` frames.
- `ChannelPlan.Duration = T / fps`. It equals the sum of segment durations. It can differ from `Metadata.Duration` by up to half a frame.
- `duration` is `Metadata.Duration`, the same value the VOD planner uses (already clamped to the video end by the prober).

### 4.2 Contract between plan and encoder

The encoder must make the output match the plan, not the other way round:

- video that ends early repeats its last frame, audio that ends early is padded with silence, anything past `T` frames is trimmed;
- every segment starts with an IDR frame and the GOP is closed and fixed, so output does not depend on where a batch starts;
- the worker validates each segment end against the plan with the existing `Worker.checkEnd`. A mismatch fails the job.

Segment times are item-relative and start at 0. Output PTS starts at the existing 10 s offset for every item, so frames must emit `EXT-X-DISCONTINUITY` at every item boundary, including an item that follows itself.

### 4.3 Determinism and a changed probe

- Metadata is stored once per media key and reused. The plan is a pure function of that stored duration and the profile, so every replica computes the same plan.
- A changed source changes the revision, hence the media key. Plan and segments start empty together.
- `Invalidate` deletes metadata and segments together.
- Remaining case: metadata is re-probed under an unchanged media key (corrupt stored metadata, or a host `SourceRevision` that does not change when the bytes do) and yields another duration. Segments `0..count-2` are unaffected because they do not depend on the duration. A stale last segment, or stale segments past the new end, can remain. This is accepted and documented: the revision must change when the bytes change. Putting `T` in the rendition hash would close it, at the price of a metadata read on every `ChannelSegment` cache hit. See risk R4.

### 4.4 `ChannelPlan` semantics

- Resolves the media key, loads metadata (probing on first use, bounded by `ProbeTimeout`), validates the profile and the audio track, returns the plan.
- Returns `ErrRenditionNotFound` when the source has no video stream, `ErrSegmentOutOfRange` when the duration is not positive, and an error wrapping `ErrTonemapUnavailable` for an HDR source on a host without `zscale`/`tonemap` (same check as the video `VariantPlaylist`).
- Enqueues nothing. Cost is one metadata read and decode, which includes the keyframe arrays. Frames should call it once per schedule entry and keep the result.

## 5. Fetch (D4)

`ChannelSegment` is `Segment` with a channel request:

1. Resolve the media key and compute the rendition name (pure, no metadata needed).
2. `Storage.SegmentExists`, then `ReadSegment` on a hit. This is the whole hot path: no metadata read.
3. On a miss: load metadata, compute the plan, check `index`, pick the batch, `joinFlight`, launch, wait up to `SegmentTimeout`. Identical events and errors to `Segment` (`ErrSegmentOutOfRange`, `ErrSegmentTimeout`, `segment error: ...`).

A `ChannelSegment` miss means the lead ran out. It joins a prefetch flight when one covers the index, otherwise it starts a batch at `index` with `PriorityChannel`.

Batch selection for channel requests, shared by fetch and prefetch:

```
batchEnd(i) = min(count-1, (i / B + 1) * B - 1)      B = ChannelSegmentsPerJob
end         = batchEnd(i), cut before the first stored segment after i
```

Batch ends sit on a fixed grid. Two replicas, or two calls a few seconds apart, therefore compute the same `(start, end)` and the same job ID, which is what makes `AcquireJob` dedup effective.

## 6. Prefetch (D5)

### 6.1 Semantics

`ChannelPrefetch(ctx, sourceURL, profile, audioTrack, from, count)` makes sure every segment in `[from, from+count)` is stored, in a local flight, or owned by another replica's job. It never waits for a transcode.

- `count <= 0` returns nil. `from < 0` or `from >= len(plan.Segments)` returns `ErrSegmentOutOfRange`. A range past the end is clamped silently, so frames can pass a constant lead.
- It returns after the needed jobs are enqueued. The work it does synchronously is bounded I/O: one metadata read (a probe on the very first call for a source), `SegmentExists` for indices not covered by a local flight, and per new batch `AcquireJob`, `WaitJob` and `Enqueue`.
- nil means "nothing more to do for this range now". It does not mean the segments are ready.
- It returns the first error and stops. Batches already launched stay launched.
- A caller that cancels `ctx` does not leave a half-built flight: launches run under `flightSetupContext`, as today.
- goshl holds no playhead and no timer. When frames stops calling, no new work starts. Jobs already queued finish; that is at most the lead plus one batch per channel.

### 6.2 Engine changes

`segments.go:segmentOnce` today is one function: cache check, plan, `joinFlight` (line 103), launch (lines 117 to 201), wait. The launch part is extracted unchanged into a helper, and the request arguments become a struct.

```go
type segmentRequest struct {
	sourceURL  string
	mediaKey   string
	streamType domain.StreamType
	rendition  string
	priority   domain.JobPriority
	segments   []domain.Segment
	video      *domain.VideoRendition
	audio      *domain.AudioRendition
	channel    *domain.ChannelSpec
	meta       *domain.Metadata
}

type launchOutcome struct {
	held        bool
	activeRunID string
	stored      bool
}

func (e *Engine) launchFlight(ctx context.Context, req segmentRequest, created *flight, index int) (launchOutcome, error)
func (e *Engine) coveringFlight(mediaKey string, streamType domain.StreamType, rendition string, index int) *flight
func (e *Engine) prefetchRange(ctx context.Context, req segmentRequest, from, count, batch int) error
```

`launchFlight` is lines 117 to 201 of `segmentOnce` minus the three `waitForSegment` calls and the `WaitSegment` subscriptions:

1. `AcquireJob(setupCtx, jobID, runID, lockTTL)`.
2. On `ErrLockHeld`: return `{held: true, activeRunID}`. The caller decides whether to wait.
3. On success: `setFlightLock`, re-check storage for `index` (return `{stored: true}` after `abandonFlight`), `WaitJob` on the lifecycle context, `setFlightWatch`, `buildJob`, `startFlightWatch`, `markEnqueued`, `Enqueue`, `EventJobEnqueued`.
4. Every existing failure branch keeps its `abandonFlight` call.

`segmentOnce` becomes: cache check, plan, `joinFlight`, `launchFlight`, then the existing wait branches. Its behaviour does not change; the existing flight tests are the regression net for the extraction.

`prefetchRange` loop, with `i` starting at `from`:

| Step | Action |
|---|---|
| 1 | `coveringFlight(i)` under the registry lock. A live flight covers `i`: set `i = flight.end + 1`, continue. No storage call. |
| 2 | `SegmentExists(i)`. Stored: `i++`, continue. |
| 3 | Compute `end` as in section 5. `joinFlight(mediaKey, StreamChannel, rendition, i, end)`. |
| 4 | `existing != nil` (lost a race with another caller): `i = existing.end + 1`, continue. |
| 5 | `created != nil`: `launchFlight`. Held by another replica: `abandonFlight(created)`, `i = created.end + 1`. Stored: `i++`. Enqueued: `i = created.end + 1`. Error: return it. |

No `WaitSegment` or request-scoped `WaitJob` is opened, so nothing is left to unsubscribe.

### 6.3 Lock and flight semantics

- A prefetch flight is an ordinary flight. It has no waiter, which the engine already supports ("a waiter that leaves does not cancel the job").
- Its lifetime is owned by the `watchFlight` goroutine that `startFlightWatch` starts under the lifecycle: a terminal `WaitJob` status calls `finishFlight` and `ReleaseJob`; otherwise `expireFlight` fires at the deadline (2 x `JobTimeout`). The batch lock TTL stays `StartBy + JobTimeout + 1 minute`.
- `Stop` behaves as today: `finishAllFlights` releases locks of flights never enqueued, enqueued ones expire by TTL.
- `joinFlight` holds the registry mutex while it looks up and inserts, so two concurrent prefetch calls on one replica cannot both create a flight for the same index.
- `joinFlight` already ends a new flight before the next live flight of the same rendition, so batches never overlap locally.
- A `ChannelSegment` request that arrives while a prefetch flight covers its index takes the existing "join" branch: `WaitSegment`, storage re-check, `waitForSegment` on the flight.
- If such a request joined a flight that prefetch then abandons (lock held elsewhere), it gets `errFlightAbandoned`, re-enters through the existing loop in `Segment` (`maxFlightReentries`), hits `ErrLockHeld` itself and waits on the owner's run. No new code.

### 6.4 Called every few seconds with an overlapping range

Lead 30 s, `B` = 10, 2 s segments, tick every 4 s:

| Tick | Range | Result |
|---|---|---|
| 1 | 100 to 114 | 100 to 109 missing: job A (100 to 109). 110 to 114 missing: job B (110 to 119, the grid end). |
| 2 | 102 to 116 | 102 to 116 covered by flights A and B. Zero storage calls, zero coordinator calls. |
| 3 to 5 | up to 110 to 124 | 120 not covered: job C (120 to 129) once the range reaches 120. |
| later | 112 to 126 | Flight A finished. 112 to 119 covered by B, 120 to 126 by C. |

- One ffmpeg start per `B` segments, not one per tick.
- The effective lead is between the requested lead and the requested lead plus one batch.
- After a flight finishes, its indices cost one `SegmentExists` each per tick until the range moves past them.
- Another replica ticking the same channel computes the same job IDs. Its `AcquireJob` returns `ErrLockHeld`, it abandons its local flight and moves on. It repeats that one coordinator call per held batch per tick until the segments are stored.
- If two replicas see different storage states and trim differently, the IDs differ and both jobs run. Segments are written twice with equivalent content. This is the same exposure VOD has today.

### 6.5 Failures

Without a guard, a source that always fails would get a new failing job on every tick. A small negative cache mirrors `metadataState`:

- `watchFlight` records `(mediaKey, rendition) -> error` for 10 s when a flight ends failed or cancelled (bounded to 1024 entries).
- `prefetchRange` checks it first and returns `segment error: <cause>` without enqueueing.
- `ChannelSegment` ignores it, so a viewer request always gets a real attempt.
- `Invalidate` clears it for the media key.

## 7. Priority and isolation (D6, D7)

### 7.1 Dedicated lane

`StreamChannel` is a new routing value. `engine.New` builds a third `transcode.Pool` with `ChannelPoolSize` workers that calls `Coordinator.Subscribe(ctx, StreamChannel, profile)`. The existing rule "a job goes only to a subscriber with its exact stream type and profile" then gives channel jobs reserved workers. A saturated VOD video or audio queue cannot delay them, whatever ordering the coordinator implements. This is the guarantee frames asked for; priority alone cannot give it, because priority never preempts a running job.

Coordinator impact: none if it treats `StreamType` as an opaque routing key. A coordinator that enumerates stream types must add `"channel"`.

### 7.2 `Job.Priority`

```go
type Job struct {
	...
	Priority JobPriority
	Channel  *ChannelSpec
	Digest   string
}

type ChannelSpec struct {
	FormatVersion    int
	Height           int
	Width            int
	FPS              int
	SegmentDuration  float64
	AudioTrack       int
	AudioStreamIndex int
	TotalFrames      int
}
```

- Both fields are covered by `JobDigest`, which already hashes every field except `EnqueuedAt` and `StartBy`. A coordinator that drops `Priority` gets `digest mismatch` rejections instead of silently unordered jobs.
- `Priority` is not part of the job ID. The same batch is the same lock at any priority.
- The job contract version becomes 6.

How the engine sets it:

| Producer | Priority |
|---|---|
| `ChannelSegment` miss, `ChannelPrefetch` | `PriorityChannel` (200) |
| `Segment` | `PriorityPlayback` (100) |
| Nothing in goshl today | `PriorityBackground` (0), reserved for a later VOD prefetch |

Values are spaced so levels can be added. Hosts must compare numerically and never switch on the constants.

Coordinator rules to add to `docs/operations.md`:

- Among pending jobs of one `(StreamType, Profile)` route, deliver the highest `Priority` first, and the earliest enqueued first within one priority.
- A redelivered job keeps its priority and goes ahead of new jobs of the same priority.
- Priority never cancels or preempts a delivered job.
- Do not buffer the `Subscribe` channel. A buffered channel is a FIFO the coordinator can no longer reorder. With an unbuffered channel at most one already-chosen job per subscription sits ahead of a newer, more urgent one.
- A coordinator that shares capacity across routes should apply the same order across them.

Starvation is bounded by `StartBy`: a job that waits past it fails (`EventJobFailed`) and is retried by the next request.

### 7.3 In-process pools

No ordering is needed inside goshl. A pool is N goroutines that each pull one delivery when free (`pool.go:worker`); there is no local queue to reorder. Ordering belongs to the coordinator.

What the lane does not isolate is the machine. Channel, video and audio workers and the in-process generators (sprites, previews, subtitles, which run ffmpeg outside any pool and without a concurrency cap) share CPU, GPU and encoder sessions. See risk R2.

### 7.4 Contract 6 and the cache (D8)

`domain.JobContractVersion` is an input to `pipelineIdentity` (`identity.go:88`), hence to every media key. Bumping it to 6 as-is makes the whole VOD cache cold, although no VOD output byte changes.

Proposed: separate the two roles the pipeline hash plays.

| Value | Hash of | Used for |
|---|---|---|
| `pipelineVersion` (cache identity) | unchanged inputs, with the contract input pinned at 5 and renamed `domain.OutputContractVersion` | media keys |
| `routeProfile` | `hashIdentity("goshl-route", pipelineVersion, JobContractVersion)` | `Job.Profile`, `Subscribe`, pool profile check |

- Media keys, asset keys and the golden identity tests stay unchanged. The cache stays warm.
- Old and new workers never exchange jobs, because the route differs.
- Old and new replicas compute equal job IDs for VOD batches, so the batch lock keeps deduplicating across a rolling deploy.
- `OutputContractVersion` is bumped only when a contract change alters output.

Alternative: bump the single version and accept a cold cache, as in v0.0.26 and v0.0.27. Simpler code, full re-transcode of the library on demand.

## 8. Readiness (D9)

Confirmed: no new API is needed.

- Existence: frames calls its own `Storage.SegmentExists(ctx, plan.SegmentData(i))`. `ChannelPlan` supplies the media key and the rendition name.
- Push: frames taps its own `Coordinator.NotifySegment`, or `WaitSegment`.

Verified in the code:

- Every segment a worker produces is written by `Worker.uploadSegment` (`internal/transcode/worker.go:245`) through `WorkerSpec.Storage`.
- Both pools receive `segment.NotifyingStorage` as that storage (`internal/engine/engine.go:104` to `124`).
- `NotifyingStorage.WriteSegment` (`internal/segment/notifying_storage.go:44`) calls `Coordinator.NotifySegment` with `SegmentStateReady` after each successful write. It is the only `NotifySegment` call in the module.
- The path does not look at the rendition or the stream type, so channel segments are notified as long as the channel pool is built with the same `notifyingStorage`. That is an explicit requirement of this design and has a test (section 10).

Limits frames must respect:

- No notification for a segment that was already stored. A job whose segments all exist completes without writing (`pool.go:segmentsExist`).
- Notifications are transient and not replayed. A failed publish only emits `EventSegmentNotifyFailed`; the segment is still stored.
- goshl never publishes `SegmentStateError`. Failures travel through job status.
- Cross-replica delivery depends on frames' coordinator fan-out.

So: check storage first, treat the notification as a wake-up, keep a slow poll as a fallback. Stored segments are immutable until `Invalidate`, so a positive result can be memoised.

## 9. Engine integration points

| File: function | Change |
|---|---|
| `api.go`, `controller.go`, `options.go`, `errors.go` | New aliases, constants, three Controller methods, two options, two errors. |
| `internal/domain/types.go` | `JobPriority`, `Job.Priority`, `Job.Channel`, `ChannelSpec`. |
| `internal/domain/playlist.go` | `StreamChannel`. |
| `internal/domain/timeline.go` | `JobContractVersion = 6`, `OutputContractVersion = 5`, `ChannelFormatVersion = 1`. |
| `internal/domain/channel.go` (new) | `ChannelProfile`, `ChannelPlan`, normalisation and validation. |
| `internal/playlist/segment.go` | `CalculateChannelSegments`. |
| `internal/rendition/name.go` | `ChannelName(profile, audioTrack)`, `IsChannelName`. |
| `internal/rendition/rendition.go` | `Channel(meta, profile, audioTrack)` returning the video and audio renditions of a channel job. |
| `internal/engine/channel.go` (new) | `Engine.ChannelPlan`, `ChannelSegment`, `ChannelPrefetch`, `prefetchRange`, prefetch failure cache. |
| `internal/engine/segments.go: Segment` | Reject `channel-` names. |
| `internal/engine/segments.go: segmentOnce` | Split into request building, `launchFlight` and the wait. |
| `internal/engine/segments.go: buildJob` | Take `segmentRequest`; set `Priority`, `Channel`, `Profile = routeProfile`. |
| `internal/engine/flights.go` | `coveringFlight`; `watchFlight` records failed runs for the prefetch cache. |
| `internal/engine/identity.go: pipelineVersion` | Pin the output contract input; add `routeProfile`. |
| `internal/engine/engine.go: New, Start, Stop` | Third pool with `notifyingStorage`, started and stopped with the others when `ChannelPoolSize > 0`. |
| `internal/engine/config.go` | Defaults and bounds of the two options. |
| `internal/transcode/pool.go: processJob` | Third branch for `StreamChannel` calling `cmdBuilder.Channel`; `WorkerSpec.IsVideo = true`. |
| `internal/transcode/pool.go: validateJob, validateRendition` | Channel jobs need both renditions, `Channel` set, both rendition names equal to `Job.Rendition`, no `Trailing`. |
| `internal/ffmpeg/command.go` | `ChannelParams`, `CommandBuilder.Channel` (format prototype). |
| `docs/*`, `MIGRATION.md`, `CHANGELOG.md` | API, configuration, coordinator contract, migration steps. |

## 10. Contract and cache impact

| Area | Impact |
|---|---|
| Job contract | 5 to 6. New `Job.Priority`, `Job.Channel`, stream type `channel`. |
| Coordinator (frames) | Carry the two new fields, route `channel`, order by `Priority`, unbuffered subscription. Deploy before goshl. |
| Cache schema | Stays `goshl-cache-v6`. |
| VOD cache | Warm with D8. Cold without it. |
| Channel cache | New, under existing media keys. |
| `Storage`, `PathGenerator` | No interface change. |
| Hosts not using channels | `ChannelPoolSize` 0: no new subscription. They still need the contract 6 coordinator change. |
| Rolling deploy | Old and new workers are on different routes. Drain or let old jobs expire. |

## 11. Test plan

Unit:

- `CalculateChannelSegments` table: exact multiple, remainder at 0.5 s, remainder under 0.5 s (merge), item shorter than one segment, shorter than 0.5 s, 25/30/50/60 fps, 100-hour item, sum of durations equals `T / fps`, every pair passes `domain.Adjacent`.
- Profile normalisation: zero value equals explicit defaults, each bound, non-integer `FPS * SegmentDuration`.
- `rendition.ChannelName`: golden values, passes `ValidName`, differs per field and per audio track and per format version, never equals a VOD name (property test over `GenerateVideo` and `GenerateAudioTracks` output).
- Identity golden tests: media keys unchanged, `routeProfile` golden, channel job ID golden.
- `JobDigest`: the existing reflective test picks up `Priority` and `Channel`; add an explicit "dropping Priority changes the digest".
- `validateJob`: channel job accepted; missing `Channel`, missing audio rendition, wrong stream type and `Trailing` set are rejected.

Engine, with the existing doubles (`doubles_test.go`):

- `ChannelPrefetch` returns while the coordinator never delivers; exactly the expected jobs were enqueued with grid-aligned ranges and `PriorityChannel`.
- Repeating the call, and calling with a shifted overlapping range, enqueues nothing new and makes no storage call for covered indices.
- Fully stored range: no coordinator call. Partly stored batch: trimmed before the first stored segment.
- Lock held by another replica: flight abandoned, nil returned, no `ReleaseJob` for a token never held.
- `ChannelSegment` joins a prefetch flight and returns when the segment is written.
- A waiter on a flight that prefetch abandons re-enters and waits on the owner.
- Failed run: next prefetch within 10 s returns the error and enqueues nothing; after 10 s it retries; `Invalidate` clears it.
- `Stop` during prefetch: unenqueued locks released, `ErrControllerStopped` returned, no goroutine left (lifecycle wait).
- Flight expiry without any waiter releases the lock.
- `Segment` regression: the whole existing `flights_test.go`, `flights_races_test.go`, `segments_test.go` and `cluster_test.go` pass unchanged after the `launchFlight` extraction.
- `Segment` and `VariantPlaylist` reject `channel-` names.
- `ChannelPoolSize` 0: all three methods return `ErrChannelDisabled`, no third `Subscribe`.

Cluster (`cluster_test.go` pattern, two engines on one coordinator):

- Both prefetch the same range: one job per batch.
- A channel job is delivered while every video worker is blocked on a VOD job.
- Channel pool writes trigger `NotifySegment` for every produced index and none for dropped overlap segments.

Race: the new prefetch tests under `-race`, plus concurrent `ChannelPrefetch` and `ChannelSegment` on the same range.

End to end (real ffmpeg, `e2e/`):

- Every produced segment has the planned duration within one frame (ffprobe).
- Batch invariance: segments 0 to 19 from one job and from two jobs concatenate to the same timeline with no gap or overlap in video or audio.
- A source with early-ending video and one with early-ending audio produce a full-length last segment.
- A source without audio produces silence.
- Two items concatenated with a discontinuity play through.
- `public_contract_test.go` compiles a host using the new symbols.

## 12. Risks

| # | Risk | Mitigation |
|---|---|---|
| R1 | Realtime: one active channel needs encode speed above 1x plus seek cost per batch. | Size `ChannelPoolSize` from the speed prototype. Grid batches keep one ffmpeg start per 20 s of media. |
| R2 | Machine contention: lanes isolate queue slots, not CPU, GPU or NVENC sessions. Unbounded in-process sprite and subtitle generation can slow channel encodes. | Size pools so the sum fits the encoder session limit. Consider a concurrency cap on the misc generators as a follow-up. |
| R3 | Audio continuity at batch joins: a re-encoded AAC stream restarted mid-item needs priming handled, as VOD audio does with `Overlap`. | Format prototype must prove batch invariance (e2e test above). `Job.Overlap` is available if it needs a discarded lead-in segment. |
| R4 | Stale tail after a re-probe under an unchanged media key (section 4.3). | Documented invariant. Option to hash `T` into the rendition if frames sees it. |
| R5 | A batch queued at channel priority cannot be raised when a viewer blocks on it. | All channel jobs share one priority and one lane, so the queue is short. |
| R6 | Wasted work: when viewers leave, queued prefetch jobs still run. | Bounded by lead plus one batch per channel. The output stays cached for reruns. |
| R7 | `launchFlight` extraction touches the most delicate code in the engine. | Pure extraction, no behaviour change, existing race and cluster suites as the gate. |
| R8 | A coordinator with a buffered subscription silently loses ordering. | Contract text and a conformance test in `coordtest`. Isolation still holds through the lane. |
| R9 | Many channels ticking: `SegmentExists` calls grow with channels x lead. | Flight coverage skips most. A batched existence check on `Storage` is a later option. |
| R10 | Hosts that parse rendition names or enumerate stream types break. | Migration note. |

## 13. Live sources (later phase, sketch only)

Not built on this design. A live input has no duration, no seek and no deterministic plan, so flights, batches and prefetch do not apply.

- A separate subsystem, `internal/live`, with its own Controller surface: `StartLive(ctx, id, inputURL, profile) error`, `StopLive(id)`, `LiveSegment(ctx, id, sequence)`, `LiveWindow(id) (firstSequence int, segments []Segment)`.
- One long-running ffmpeg per live input, owned by one replica through a renewed coordinator lock, restarted on failure with a sequence gap.
- Segments are numbered by a monotonic sequence, stored under a `live-...` namespace with a retention window instead of a cache.
- It reuses the channel output format, so a channel can switch between a file item and a live item with a discontinuity.
- It reuses `NotifyingStorage` for readiness. It does not use the job queue.
- Input protocol whitelist, reconnection and clock handling need their own design.

## 14. Open questions for frames

1. Tail rule: is "remainder under 0.5 s merges, otherwise a short last segment" acceptable, or do you prefer every segment padded to exactly `SegmentDuration` (item length becomes `count * SegmentDuration`, up to one segment of black at the end)?
2. `audioTrack` as the position in `Metadata.Audios`: correct for you, or do you hold container stream indices?
3. Is one profile per channel enough for now, and is a multi-rendition channel planned? (Per-call profiles already allow it.)
4. Does your coordinator treat `StreamType` as opaque, and is its subscription channel buffered?
5. What is "background work" on your side: goshl sprites, previews and subtitles, or your own library pre-transcode through `Segment`? The second would use a VOD `Prefetch` at `PriorityBackground`, which falls out of the same code.
6. Does your `Storage` parse `SegmentData.Rendition`, or store it opaquely?
7. Tick interval and lead you plan to use, and how many channels can be watched at once (sizes `ChannelPoolSize`).
8. Would a readiness by-product from `ChannelPrefetch` (number of contiguous stored segments from `from`) save you storage calls, or do you prefer the error-only signature?
9. Do you guarantee that `SourceRevision` changes whenever the bytes change (risk R4)?
10. Should a queued channel job that has not started within the lead time be dropped (shorter `StartBy` for channel jobs)?

## 15. Open decisions for Roy

1. D8: split the routing profile from the cache identity (warm VOD cache) or accept a cold cache.
2. `ChannelPoolSize` default 0 (opt-in) as proposed.
3. Ship a VOD `Prefetch` at `PriorityBackground` in the same release or leave the constant reserved.
