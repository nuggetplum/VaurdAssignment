package db

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nuggetplum/VaurdAssignment/api"
	"github.com/nuggetplum/VaurdAssignment/models"
)

// Repository provides persistence for orders backed by Postgres.
type Repository struct {
	pool *pgxpool.Pool

	// invalidTransitions counts status-update events rejected by the D4
	// state machine guard. Logged and counted, per plan.md D4 — never dropped
	// silently.
	invalidTransitions atomic.Int64
}

// NewRepository wraps an existing connection pool.
func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// Ping reports whether the database is reachable, for GET /healthz.
func (r *Repository) Ping(ctx context.Context) error {
	return r.pool.Ping(ctx)
}

// applyEventSQL is a single UPSERT that both inserts a brand-new order row and
// merges an event's fields into an existing one.
//   - COALESCE(EXCLUDED.col, orders.col) merges only the columns this event
//     actually carries; a nil parameter leaves the existing value untouched.
//   - The WHERE clause enforces last-write-wins by event time (plan §3.2) AND
//     the D4 status state machine, as one atomic condition. If either check
//     fails, the whole row update is skipped (rows affected = 0).
const applyEventSQL = `
INSERT INTO orders (order_id, customer_id, restaurant_id, status, items, last_event_at, last_updated_at)
VALUES ($1, $2, $3, $4, $5::jsonb, $6, now())
ON CONFLICT (order_id) DO UPDATE SET
    customer_id     = COALESCE(EXCLUDED.customer_id, orders.customer_id),
    restaurant_id   = COALESCE(EXCLUDED.restaurant_id, orders.restaurant_id),
    status          = COALESCE(EXCLUDED.status, orders.status),
    items           = COALESCE(EXCLUDED.items, orders.items),
    last_event_at   = EXCLUDED.last_event_at,
    last_updated_at = now()
WHERE EXCLUDED.last_event_at > orders.last_event_at
  AND (
        EXCLUDED.status IS NULL
     OR orders.status IS NULL
     OR EXCLUDED.status = orders.status
     OR (orders.status = 'Received'  AND EXCLUDED.status IN ('Preparing', 'Cancelled'))
     OR (orders.status = 'Preparing' AND EXCLUDED.status IN ('Complete', 'Cancelled'))
  )
`

// ApplyEvent merges a single order event into the current-state table. It is
// safe to call concurrently for different orders, and safe to call with
// duplicate or out-of-order events for the same order: the SQL guard above
// turns replays and stale events into no-ops instead of corrupting state.
func (r *Repository) ApplyEvent(ctx context.Context, event models.OrderEvent) error {
	var customerID, restaurantID, status *string
	var itemsJSON []byte

	switch event.EventType {
	case models.EventOrderCreate:
		customerID = &event.CustomerID
		restaurantID = &event.RestaurantID
		// A create event unambiguously means "this order was just Received" —
		// not "unknown, ask again later" like the other fields can be. If a
		// create arrives late for an order that already has a newer status
		// (the update-before-create defensive path, §3.7), the LWW guard
		// still protects it: a create's occurredAt is always earlier than any
		// status change on that order, so `EXCLUDED.last_event_at >
		// orders.last_event_at` fails first and the whole row update
		// (including this baseline) is skipped.
		receivedStatus := models.StatusReceived
		status = &receivedStatus
		b, err := json.Marshal(event.Items)
		if err != nil {
			return fmt.Errorf("marshal items for event %s: %w", event.EventID, err)
		}
		itemsJSON = b
	case models.EventOrderUpdateStatus:
		status = &event.Status
	case models.EventOrderUpdateItems:
		b, err := json.Marshal(event.Items)
		if err != nil {
			return fmt.Errorf("marshal items for event %s: %w", event.EventID, err)
		}
		itemsJSON = b
	default:
		return fmt.Errorf("unknown event type %q for event %s", event.EventType, event.EventID)
	}

	// A nil itemsJSON must reach pgx as an untyped nil (SQL NULL), not the
	// zero value of []byte cast to string.
	var itemsParam any
	if itemsJSON != nil {
		itemsParam = string(itemsJSON)
	}

	tag, err := r.pool.Exec(ctx, applyEventSQL,
		event.OrderID, customerID, restaurantID, status, itemsParam, event.OccurredAt,
	)
	if err != nil {
		return fmt.Errorf("apply event %s: %w", event.EventID, err)
	}

	// The UPSERT can't tell us *why* it had no effect (stale replay vs.
	// invalid transition look identical from rows-affected alone). Only
	// status-update events can hit the state-machine guard, and this no-op
	// path is rare, so one extra read here to log accurately is cheap.
	if tag.RowsAffected() == 0 && event.EventType == models.EventOrderUpdateStatus {
		r.logInvalidOrStaleStatusUpdate(ctx, event)
	}

	return nil
}

// logInvalidOrStaleStatusUpdate distinguishes "stale/duplicate event" (LWW
// guard, expected and harmless) from "genuinely invalid transition" (D4:
// must be logged and counted) for a status-update event that caused no row
// change. Timestamp is checked FIRST: if this event isn't even newer than
// what we have, the LWW guard alone explains the no-op, regardless of
// whether the transition would also have been invalid.
func (r *Repository) logInvalidOrStaleStatusUpdate(ctx context.Context, event models.OrderEvent) {
	var currentStatus *string
	var lastEventAt time.Time
	err := r.pool.QueryRow(ctx, `SELECT status, last_event_at FROM orders WHERE order_id = $1`, event.OrderID).
		Scan(&currentStatus, &lastEventAt)
	if err != nil {
		log.Printf("event %s: had no effect, and failed to determine why: %v", event.EventID, err)
		return
	}

	if !event.OccurredAt.After(lastEventAt) {
		log.Printf("stale/duplicate status update ignored: order=%s event=%s occurredAt=%s lastEventAt=%s",
			event.OrderID, event.EventID, event.OccurredAt, lastEventAt)
		return
	}

	// The event is newer in time, so the only remaining reason it could have
	// no-op'd is the state machine guard.
	if currentStatus != nil && !isValidTransition(*currentStatus, event.Status) {
		r.invalidTransitions.Add(1)
		log.Printf("invalid status transition rejected: order=%s from=%s to=%s event=%s",
			event.OrderID, *currentStatus, event.Status, event.EventID)
		return
	}

	log.Printf("event %s: had no effect for an unexpected reason (order=%s)", event.EventID, event.OrderID)
}

// isValidTransition mirrors the state machine encoded in applyEventSQL's
// WHERE clause. It is used only to produce an accurate log message; the
// actual enforcement happens atomically in SQL.
func isValidTransition(from, to string) bool {
	if from == to {
		return true
	}
	switch from {
	case models.StatusReceived:
		return to == models.StatusPreparing || to == models.StatusCancelled
	case models.StatusPreparing:
		return to == models.StatusComplete || to == models.StatusCancelled
	default:
		// Complete and Cancelled are terminal: nothing transitions out of them.
		return false
	}
}

// InvalidTransitionCount returns the number of status-update events rejected
// by the D4 state machine guard since startup.
func (r *Repository) InvalidTransitionCount() int64 {
	return r.invalidTransitions.Load()
}

// listOrdersSQL selects the current page of orders plus, via COUNT(*) OVER(),
// the total row count matching the filter in the same query.
const listOrdersSQL = `
SELECT order_id, customer_id, restaurant_id, status, items, last_updated_at, COUNT(*) OVER() AS total
FROM orders
WHERE ($1 = '' OR status = $1)
ORDER BY last_updated_at `

// ListOrders returns one page of current order state, filtered by status
// (optional) and sorted/paginated per q. q.Sort/Limit/Offset are assumed
// already validated by the HTTP layer.
func (r *Repository) ListOrders(ctx context.Context, q api.ListOrdersQuery) ([]models.Order, int, error) {
	direction := "DESC"
	if q.Sort == api.SortLastUpdatedAsc {
		direction = "ASC"
	}

	query := listOrdersSQL + direction + ` LIMIT $2 OFFSET $3`

	rows, err := r.pool.Query(ctx, query, q.Status, q.Limit, q.Offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list orders: %w", err)
	}
	defer rows.Close()

	orders := []models.Order{}
	total := 0

	for rows.Next() {
		var (
			o         models.Order
			status    *string
			itemsJSON []byte
		)
		if err := rows.Scan(&o.OrderID, &o.CustomerID, &o.RestaurantID, &status, &itemsJSON, &o.LastUpdated, &total); err != nil {
			return nil, 0, fmt.Errorf("scan order row: %w", err)
		}

		if status != nil {
			o.Status = *status
		}

		o.Items = []models.OrderItem{}
		if itemsJSON != nil {
			if err := json.Unmarshal(itemsJSON, &o.Items); err != nil {
				return nil, 0, fmt.Errorf("unmarshal items for order %s: %w", o.OrderID, err)
			}
		}

		orders = append(orders, o)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate order rows: %w", err)
	}

	return orders, total, nil
}
