package models

import "time"

// EventType identifies which of the three event shapes an OrderEvent carries.
type EventType string

const (
	EventOrderCreate       EventType = "order.create"
	EventOrderUpdateStatus EventType = "order.update.status"
	EventOrderUpdateItems  EventType = "order.update.items"
)

// Status values for the enforced order state machine (D4).
const (
	StatusReceived  = "Received"
	StatusPreparing = "Preparing"
	StatusComplete  = "Complete"
	StatusCancelled = "Cancelled"
)

// OrderEvent is the JSON envelope published to JetStream subject "orders.events".
// Which fields are populated depends on EventType:
//   - order.create:        CustomerID, RestaurantID, Items are set; OrderID is absent.
//   - order.update.status:  OrderID and Status are set.
//   - order.update.items:   OrderID and Items are set.
// EventID and OccurredAt are always required: EventID is the dedup key, and
// OccurredAt is the ordering key used for last-write-wins conflict resolution.
type OrderEvent struct {
	EventID      string      `json:"eventId"`
	EventType    EventType   `json:"eventType"`
	OccurredAt   time.Time   `json:"occurredAt"`
	OrderID      string      `json:"orderId,omitempty"`
	CustomerID   string      `json:"customerId,omitempty"`
	RestaurantID string      `json:"restaurantId,omitempty"`
	Status       string      `json:"status,omitempty"`
	Items        []OrderItem `json:"items,omitempty"`
}
