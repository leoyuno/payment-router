package main

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Error: code, Message: message})
}

// POST /v1/payments/authorize
func handleAuthorize(router *Router) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req PaymentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "Invalid JSON body")
			return
		}

		if req.Amount <= 0 {
			writeError(w, http.StatusBadRequest, "validation_error", "amount must be positive")
			return
		}
		if len(req.Currency) != 3 {
			writeError(w, http.StatusBadRequest, "validation_error", "currency must be 3-letter ISO code")
			return
		}
		if len(req.Country) != 2 {
			writeError(w, http.StatusBadRequest, "validation_error", "country must be 2-letter ISO code")
			return
		}
		if req.CustomerID == "" {
			writeError(w, http.StatusBadRequest, "validation_error", "customer_id is required")
			return
		}

		resp, err := router.Route(r.Context(), req)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "routing_failed", err.Error())
			return
		}

		writeJSON(w, http.StatusOK, resp)
	}
}

// GET /v1/processors
func handleListProcessors(router *Router) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, router.GetProcessors())
	}
}

// GET /v1/transactions
func handleListTransactions(router *Router) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := 50
		if l := r.URL.Query().Get("limit"); l != "" {
			if n, err := strconv.Atoi(l); err == nil && n > 0 {
				limit = n
			}
		}
		writeJSON(w, http.StatusOK, router.GetTransactions(limit))
	}
}

// POST /v1/admin/processors/{id}/simulate
func handleSimulate(router *Router) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		proc := router.GetProcessor(id)
		if proc == nil {
			writeError(w, http.StatusNotFound, "not_found", "processor not found: "+id)
			return
		}

		var req SimulateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "Invalid JSON body")
			return
		}

		proc.SetSimulation(req)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"processor": id,
			"simulation": req,
			"message":   "Simulation updated",
		})
	}
}

// POST /v1/admin/processors/{id}/status
func handleOverrideStatus(router *Router) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		proc := router.GetProcessor(id)
		if proc == nil {
			writeError(w, http.StatusNotFound, "not_found", "processor not found: "+id)
			return
		}

		var req StatusOverrideRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "Invalid JSON body")
			return
		}

		switch ProcessorStatus(req.Status) {
		case StatusHealthy:
			proc.ClearOverride()
			writeJSON(w, http.StatusOK, map[string]string{
				"processor": id,
				"status":    req.Status,
				"message":   "Override cleared, using computed health",
			})
		case StatusDegraded, StatusDown:
			proc.SetOverride(ProcessorStatus(req.Status))
			writeJSON(w, http.StatusOK, map[string]string{
				"processor": id,
				"status":    req.Status,
				"message":   "Manual override applied",
			})
		default:
			writeError(w, http.StatusBadRequest, "validation_error", "status must be HEALTHY, DEGRADED, or DOWN")
		}
	}
}
