package coordtest

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
)

func queued(receipt string, priority domain.JobPriority) domain.JobDelivery {
	return domain.JobDelivery{Receipt: receipt, Job: domain.Job{ID: receipt, Priority: priority}}
}

func drain(t *testing.T, queue *PriorityQueue, count int) []string {
	t.Helper()
	var order []string
	for len(order) < count {
		select {
		case delivery := <-queue.Deliveries():
			order = append(order, delivery.Receipt)
		case <-time.After(time.Second):
			t.Fatalf("only %v was delivered", order)
		}
	}
	return order
}

func TestPriorityQueueDeliversHighestPriorityFirst(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	queue := NewPriorityQueue(ctx)
	queue.Push(queued("background-1", domain.PriorityBackground))
	queue.Push(queued("playback-1", domain.PriorityPlayback))
	queue.Push(queued("background-2", domain.PriorityBackground))
	queue.Push(queued("channel-1", domain.PriorityChannel))
	queue.Push(queued("playback-2", domain.PriorityPlayback))
	queue.Push(queued("channel-2", domain.PriorityChannel))
	queue.Push(queued("between", 150))
	time.Sleep(20 * time.Millisecond)
	want := []string{"channel-1", "channel-2", "between", "playback-1", "playback-2", "background-1", "background-2"}
	if got := drain(t, queue, len(want)); !reflect.DeepEqual(got, want) {
		t.Fatalf("delivery order %v, want %v", got, want)
	}
}

func TestPriorityQueueReordersUntilAWorkerTakesTheJob(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	queue := NewPriorityQueue(ctx)
	queue.Push(queued("playback-1", domain.PriorityPlayback))
	time.Sleep(20 * time.Millisecond)
	queue.Push(queued("channel-1", domain.PriorityChannel))
	time.Sleep(20 * time.Millisecond)
	if got := drain(t, queue, 2); !reflect.DeepEqual(got, []string{"channel-1", "playback-1"}) {
		t.Fatalf("a job that no worker has taken must not block a more urgent one: %v", got)
	}
}

func TestPriorityQueueRedeliveryGoesAheadOfItsOwnPriority(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	queue := NewPriorityQueue(ctx)
	queue.Push(queued("playback-1", domain.PriorityPlayback))
	queue.Push(queued("playback-2", domain.PriorityPlayback))
	queue.Push(queued("channel-1", domain.PriorityChannel))
	queue.Redeliver(queued("playback-0", domain.PriorityPlayback))
	time.Sleep(20 * time.Millisecond)
	want := []string{"channel-1", "playback-0", "playback-1", "playback-2"}
	if got := drain(t, queue, len(want)); !reflect.DeepEqual(got, want) {
		t.Fatalf("delivery order %v, want %v", got, want)
	}
}
