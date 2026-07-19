package broker

import (
	"context"
	"hash/fnv"
	"log"
	"sync"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/nuggetplum/VaurdAssignment/models"
)

// EventHandler processes a single decoded order event, typically ending in a
// call to Repository.ApplyEvent. A non-nil error means "don't ack" — the
// message will be redelivered (at-least-once).
type EventHandler func(ctx context.Context, event models.OrderEvent) error

// Pool is a hashed worker pool: it is a throughput optimization, NOT the
// correctness mechanism (that's the conditional UPSERT in db/repository.go,
// per plan.md §3.4). Hashing order_id to a fixed worker means every event
// for the same order is handled by the same goroutine, one at a time, in
// submission order, while different orders are processed fully in parallel.
type Pool struct {
	ctx      context.Context
	handle   EventHandler
	channels []chan work
	wg       sync.WaitGroup
}

type work struct {
	msg   jetstream.Msg
	event models.OrderEvent
}

// NewPool starts `workers` goroutines, each with a buffered channel of
// capacity bufferSize, and returns a Pool ready to accept Submit calls.
func NewPool(ctx context.Context, workers, bufferSize int, handle EventHandler) *Pool {
	p := &Pool{
		ctx:      ctx,
		handle:   handle,
		channels: make([]chan work, workers),
	}
	for i := range p.channels {
		ch := make(chan work, bufferSize)
		p.channels[i] = ch
		p.wg.Add(1)
		go p.runWorker(ch)
	}
	return p
}

func (p *Pool) runWorker(ch chan work) {
	defer p.wg.Done()
	for w := range ch {
		if err := p.handle(p.ctx, w.event); err != nil {
			log.Printf("event %s failed, will redeliver: %v", w.event.EventID, err)
			_ = w.msg.Nak()
			continue
		}
		_ = w.msg.Ack()
	}
}

// Submit routes msg/event to the worker responsible for event.OrderID and
// blocks if that worker's buffer is full (backpressure).
func (p *Pool) Submit(msg jetstream.Msg, event models.OrderEvent) {
	idx := hashOrderID(event.OrderID) % uint32(len(p.channels))
	p.channels[idx] <- work{msg: msg, event: event}
}

// Close stops accepting new work and blocks until every already-submitted
// job has been handled (drains in-flight work for graceful shutdown).
func (p *Pool) Close() {
	for _, ch := range p.channels {
		close(ch)
	}
	p.wg.Wait()
}

func hashOrderID(orderID string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(orderID))
	return h.Sum32()
}
