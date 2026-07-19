package server

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"github.com/nuggetplum/VaurdAssignment/api"
	"github.com/nuggetplum/VaurdAssignment/db"
	"github.com/nuggetplum/VaurdAssignment/models"
)

const (
	defaultLimit = 50
	maxLimit     = 200
)

// Server exposes the List Orders HTTP API described in API.md.
type Server struct {
	repo *db.Repository
}

// New wires a Server to the given repository.
func New(repo *db.Repository) *Server {
	return &Server{repo: repo}
}

// Routes returns the HTTP handler for the whole API.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orders", s.handleListOrders)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	return mux
}

func (s *Server) handleListOrders(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	status := query.Get("status")
	if status != "" && !isValidStatus(status) {
		http.Error(w, "invalid status filter", http.StatusBadRequest)
		return
	}

	sort := query.Get("sort")
	if sort == "" {
		sort = api.SortLastUpdatedDesc
	}
	if sort != api.SortLastUpdatedDesc && sort != api.SortLastUpdatedAsc {
		http.Error(w, "invalid sort value", http.StatusBadRequest)
		return
	}

	limit, ok := parseNonNegativeInt(w, query, "limit", defaultLimit)
	if !ok {
		return
	}
	if limit > maxLimit {
		limit = maxLimit
	}

	offset, ok := parseNonNegativeInt(w, query, "offset", 0)
	if !ok {
		return
	}

	orders, total, err := s.repo.ListOrders(r.Context(), api.ListOrdersQuery{
		Status: status,
		Sort:   sort,
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		log.Printf("list orders: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, api.ListOrdersResponse{
		Orders: orders,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.repo.Ping(r.Context()); err != nil {
		http.Error(w, "database unreachable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func isValidStatus(status string) bool {
	switch status {
	case models.StatusReceived, models.StatusPreparing, models.StatusComplete, models.StatusCancelled:
		return true
	default:
		return false
	}
}

// parseNonNegativeInt reads an optional integer query param, writing a 400
// response and returning ok=false if it's present but not a valid
// non-negative integer.
func parseNonNegativeInt(w http.ResponseWriter, query map[string][]string, key string, fallback int) (value int, ok bool) {
	raw := ""
	if v, present := query[key]; present && len(v) > 0 {
		raw = v[0]
	}
	if raw == "" {
		return fallback, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		http.Error(w, "invalid "+key, http.StatusBadRequest)
		return 0, false
	}
	return n, true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("write json response: %v", err)
	}
}
