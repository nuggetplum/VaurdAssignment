package api

import "github.com/nuggetplum/VaurdAssignment/models"

// Valid values for the `sort` query param on GET /orders.
const (
	SortLastUpdatedDesc = "last_updated_desc"
	SortLastUpdatedAsc  = "last_updated_asc"
)

// ListOrdersQuery holds the parsed & validated query params for GET /orders.
type ListOrdersQuery struct {
	Status string // "" means no filter
	Sort   string
	Limit  int
	Offset int
}

// ListOrdersResponse is the JSON body returned by GET /orders.
type ListOrdersResponse struct {
	Orders []models.Order `json:"orders"`
	Total  int            `json:"total"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
}
