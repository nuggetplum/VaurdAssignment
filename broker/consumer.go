package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/nuggetplum/VaurdAssignment/models"
)

const (
	StreamName   = "ORDERS"
	SubjectName  = "orders.events"
	ConsumerName = "orders-service"
)

// OrderNamespace is a fixed, arbitrarily-generated UUID used to derive
// deterministic order IDs from a create event's event_id (plan.md D7):
//
//	orderId = UUIDv5(OrderNamespace, event.EventID)
//
// Generated once and hardcoded forever — changing it would change every
// derived order ID, breaking idempotency for anything already persisted.
var OrderNamespace = uuid.MustParse("258b0800-4e9e-43ee-b246-94ff45ee14b6")

// Dispatch receives a decoded event alongside the raw JetStream message it
// came from. The implementation owns acking msg (directly, or by handing it
// to a Pool).
type Dispatch func(msg jetstream.Msg, event models.OrderEvent)

// Consumer wraps a durable JetStream pull consumer bound to the ORDERS
// stream.
type Consumer struct {
	nc   *nats.Conn
	cons jetstream.Consumer
}

// NewConsumer connects to NATS at natsURL and ensures the durable stream +
// consumer exist. Both create/update calls are idempotent, so this is safe
// to call on every service startup.
func NewConsumer(ctx context.Context, natsURL string) (*Consumer, error) {
	nc, err := nats.Connect(natsURL)
	if err != nil {
		return nil, fmt.Errorf("connect to nats at %s: %w", natsURL, err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("create jetstream context: %w", err)
	}

	stream, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     StreamName,
		Subjects: []string{SubjectName},
	})
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("create/update stream %s: %w", StreamName, err)
	}

	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       ConsumerName,
		AckPolicy:     jetstream.AckExplicitPolicy,
		FilterSubject: SubjectName,
	})
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("create/update consumer %s: %w", ConsumerName, err)
	}

	return &Consumer{nc: nc, cons: cons}, nil
}

// Run consumes messages until ctx is cancelled, decoding each one and handing
// it to dispatch. Graceful shutdown: cancelling ctx stops the fetch loop
// after its current message; it does not forcibly kill in-flight handling.
func (c *Consumer) Run(ctx context.Context, dispatch Dispatch) error {
	iter, err := c.cons.Messages()
	if err != nil {
		return fmt.Errorf("start consuming: %w", err)
	}

	go func() {
		<-ctx.Done()
		iter.Stop()
	}()

	for {
		msg, err := iter.Next()
		if err != nil {
			if errors.Is(err, jetstream.ErrMsgIteratorClosed) {
				return nil
			}
			return fmt.Errorf("fetch next message: %w", err)
		}

		var event models.OrderEvent
		if err := json.Unmarshal(msg.Data(), &event); err != nil {
			log.Printf("dropping unparseable event (terminated, no redelivery): %v", err)
			_ = msg.Term()
			continue
		}

		if event.EventID == "" {
			log.Printf("dropping event with no eventId (terminated, no redelivery)")
			_ = msg.Term()
			continue
		}

		if event.EventType == models.EventOrderCreate {
			event.OrderID = uuid.NewSHA1(OrderNamespace, []byte(event.EventID)).String()
		} else if event.OrderID == "" {
			log.Printf("dropping %s event with no orderId (terminated, no redelivery): event=%s",
				event.EventType, event.EventID)
			_ = msg.Term()
			continue
		}

		dispatch(msg, event)
	}
}

// Close releases the underlying NATS connection.
func (c *Consumer) Close() {
	c.nc.Close()
}
