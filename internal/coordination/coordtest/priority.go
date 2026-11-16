package coordtest

import (
	"context"
	"sync"

	"github.com/eleven-am/goshl/internal/domain"
)

type PriorityQueue struct {
	mu      sync.Mutex
	pending []domain.JobDelivery
	changed chan struct{}
	out     chan domain.JobDelivery
}

func NewPriorityQueue(ctx context.Context) *PriorityQueue {
	queue := &PriorityQueue{changed: make(chan struct{}, 1), out: make(chan domain.JobDelivery)}
	go queue.dispatch(ctx)
	return queue
}

func (q *PriorityQueue) Deliveries() <-chan domain.JobDelivery {
	return q.out
}

func (q *PriorityQueue) Push(delivery domain.JobDelivery) {
	q.insert(delivery, false)
}

func (q *PriorityQueue) Redeliver(delivery domain.JobDelivery) {
	q.insert(delivery, true)
}

func (q *PriorityQueue) insert(delivery domain.JobDelivery, ahead bool) {
	q.mu.Lock()
	position := len(q.pending)
	for index, waiting := range q.pending {
		if waiting.Job.Priority < delivery.Job.Priority || ahead && waiting.Job.Priority == delivery.Job.Priority {
			position = index
			break
		}
	}
	q.pending = append(q.pending, domain.JobDelivery{})
	copy(q.pending[position+1:], q.pending[position:])
	q.pending[position] = delivery
	q.mu.Unlock()
	select {
	case q.changed <- struct{}{}:
	default:
	}
}

func (q *PriorityQueue) head() (domain.JobDelivery, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return domain.JobDelivery{}, false
	}
	return q.pending[0], true
}

func (q *PriorityQueue) take(delivery domain.JobDelivery) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for index, waiting := range q.pending {
		if waiting.Receipt == delivery.Receipt {
			q.pending = append(q.pending[:index], q.pending[index+1:]...)
			return
		}
	}
}

func (q *PriorityQueue) dispatch(ctx context.Context) {
	for {
		next, found := q.head()
		if !found {
			select {
			case <-ctx.Done():
				return
			case <-q.changed:
				continue
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-q.changed:
		case q.out <- next:
			q.take(next)
		}
	}
}
