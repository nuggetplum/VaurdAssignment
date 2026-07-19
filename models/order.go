package models

import "time"

// OrderItem represents a single item in the food order.
// We map this to the JSONB array in our database.
type OrderItem struct {
	ItemID string `json:"itemId"`
	Qty    int    `json:"qty"`
}

// Order represents the current state of a food delivery order.
type Order struct {
	OrderID      string      `json:"orderId"`
	CustomerID   *string     `json:"customerId,omitempty"`
	RestaurantID *string     `json:"restaurantId,omitempty"`
	Status       string      `json:"status"`
	Items        []OrderItem `json:"items"`
	LastUpdated  time.Time   `json:"lastUpdated"`
}
