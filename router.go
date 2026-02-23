package main

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/sony/gobreaker"
)

// --- Types ---

type ProcessorStatus string

const (
	StatusHealthy  ProcessorStatus = "HEALTHY"
	StatusDegraded ProcessorStatus = "DEGRADED"
	StatusDown     ProcessorStatus = "DOWN"
)

type TransactionStatus string

const (
	TxnApproved TransactionStatus = "APPROVED"
	TxnDeclined TransactionStatus = "DECLINED"
	TxnError    TransactionStatus = "ERROR"
)

type PaymentRequest struct {
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	PaymentMethod string `json:"payment_method"`
	Country       string `json:"country"`
	CustomerID    string `json:"customer_id"`
	Description   string `json:"description"`
}

type RoutingDecision struct {
	Reason              string   `json:"reason"`
	ProcessorsEvaluated []string `json:"processors_evaluated"`
	FallbackUsed        bool     `json:"fallback_used"`
	GeoMatch            bool     `json:"geo_match"`
	CostBasisUSD        float64  `json:"cost_basis_usd"`
}

type PaymentResponse struct {
	TransactionID   string            `json:"transaction_id"`
	Status          TransactionStatus `json:"status"`
	ProcessorUsed   string            `json:"processor_used"`
	RoutingDecision RoutingDecision   `json:"routing_decision"`
	Amount          int64             `json:"amount"`
	Currency        string            `json:"currency"`
	ProcessorMsg    string            `json:"processor_message,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	Flow            []FlowStep        `json:"flow"`
}

type FlowStep struct {
	Step       int         `json:"step"`
	Label      string      `json:"label"`
	Direction  string      `json:"direction"` // "inbound", "internal", "outbound", "response"
	Timestamp  time.Time   `json:"timestamp"`
	DurationMs int64       `json:"duration_ms,omitempty"`
	Data       interface{} `json:"data"`
}

type ProcessorEvaluation struct {
	ProcessorID  string          `json:"processor_id"`
	Status       ProcessorStatus `json:"status"`
	CircuitState string          `json:"circuit_state"`
	GeoMatch     bool            `json:"geo_match"`
	CostPerTxn   float64         `json:"cost_per_txn"`
	Priority     int             `json:"priority"`
	Eligible     bool            `json:"eligible"`
	SkipReason   string          `json:"skip_reason,omitempty"`
}

type Transaction struct {
	ID            string            `json:"id"`
	ProcessorUsed string            `json:"processor_used"`
	Status        TransactionStatus `json:"status"`
	Amount        int64             `json:"amount"`
	Currency      string            `json:"currency"`
	Country       string            `json:"country"`
	RoutingReason string            `json:"routing_reason"`
	LatencyMs     int64             `json:"latency_ms"`
	CreatedAt     time.Time         `json:"created_at"`
}

type SimulateRequest struct {
	FailureRate float64 `json:"failure_rate"`
	ForceError  bool    `json:"force_error"`
	Active      bool    `json:"active"`
}

type StatusOverrideRequest struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

type ProcessorInfo struct {
	ID              string          `json:"id"`
	Priority        int             `json:"priority"`
	CostPerTxn      float64         `json:"cost_per_txn"`
	BaseSuccessRate float64         `json:"base_success_rate"`
	AvgLatencyMs    int             `json:"avg_latency_ms"`
	GeoPreferences  []string        `json:"geo_preferences"`
	Status          ProcessorStatus `json:"status"`
	ApprovalRate    float64         `json:"approval_rate"`
	TotalTxns       int64           `json:"total_txns"`
	TotalApproved   int64           `json:"total_approved"`
	TotalDeclined   int64           `json:"total_declined"`
	TotalErrors     int64           `json:"total_errors"`
	CircuitState    string          `json:"circuit_state"`
	Simulation      SimulateRequest `json:"simulation"`
}

// --- Processor ---

const healthWindowSize = 20

type Processor struct {
	ID              string
	Priority        int
	CostPerTxn      float64
	BaseSuccessRate float64
	AvgLatencyMs    int
	GeoPreferences  []string

	mu             sync.RWMutex
	status         ProcessorStatus
	recentResults  []bool
	approvalRate   float64
	simFailRate    float64
	simForceError  bool
	simActive      bool
	overrideActive bool
	overrideStatus ProcessorStatus
	totalTxns      int64
	totalApproved  int64
	totalDeclined  int64
	totalErrors    int64
	breaker        *gobreaker.CircuitBreaker
	rng            *rand.Rand
}

func NewProcessor(id string, priority int, cost float64, successRate float64, latencyMs int, geo []string) *Processor {
	p := &Processor{
		ID:              id,
		Priority:        priority,
		CostPerTxn:      cost,
		BaseSuccessRate: successRate,
		AvgLatencyMs:    latencyMs,
		GeoPreferences:  geo,
		status:          StatusHealthy,
		approvalRate:    1.0,
		recentResults:   make([]bool, 0, healthWindowSize),
		rng:             rand.New(rand.NewSource(time.Now().UnixNano() + int64(priority))),
	}

	p.breaker = gobreaker.NewCircuitBreaker(gobreaker.Settings{
		Name:        id,
		MaxRequests: 3,
		Interval:    30 * time.Second,
		Timeout:     10 * time.Second,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= 5
		},
	})

	return p
}

func (p *Processor) Authorize(ctx context.Context, req PaymentRequest) (approved bool, latencyMs int64, err error) {
	p.mu.RLock()
	simActive := p.simActive
	simFailRate := p.simFailRate
	simForceError := p.simForceError
	baseRate := p.BaseSuccessRate
	avgLatency := p.AvgLatencyMs
	p.mu.RUnlock()

	// Simulate latency with ±20% jitter
	jitter := 0.8 + p.rng.Float64()*0.4
	sleepMs := float64(avgLatency) * jitter
	time.Sleep(time.Duration(sleepMs) * time.Millisecond)
	latencyMs = int64(sleepMs)

	// Check context
	select {
	case <-ctx.Done():
		return false, latencyMs, ctx.Err()
	default:
	}

	// Force error simulation
	if simActive && simForceError {
		return false, latencyMs, fmt.Errorf("simulated infrastructure error on %s", p.ID)
	}

	// Determine effective success rate
	effectiveRate := baseRate
	if simActive {
		effectiveRate = 1.0 - simFailRate
	}

	// Roll the dice
	approved = p.rng.Float64() < effectiveRate
	return approved, latencyMs, nil
}

func (p *Processor) RecordResult(approved bool, isError bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.totalTxns++
	if isError {
		p.totalErrors++
	} else if approved {
		p.totalApproved++
	} else {
		p.totalDeclined++
	}

	// Only track business outcomes (not infra errors) in health window
	if !isError {
		if len(p.recentResults) >= healthWindowSize {
			p.recentResults = p.recentResults[1:]
		}
		p.recentResults = append(p.recentResults, approved)
		p.updateHealth()
	}
}

func (p *Processor) updateHealth() {
	// Need at least 5 results before making health decisions
	if len(p.recentResults) < 5 {
		p.approvalRate = 1.0
		return
	}

	approvals := 0
	for _, r := range p.recentResults {
		if r {
			approvals++
		}
	}
	p.approvalRate = float64(approvals) / float64(len(p.recentResults))

	if p.overrideActive {
		return
	}

	if p.approvalRate < 0.50 {
		p.status = StatusDown
	} else if p.approvalRate < 0.70 {
		p.status = StatusDegraded
	} else {
		p.status = StatusHealthy
	}
}

func (p *Processor) GetStatus() ProcessorStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.overrideActive {
		return p.overrideStatus
	}
	return p.status
}

func (p *Processor) SetSimulation(sim SimulateRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()

	wasActive := p.simActive
	p.simActive = sim.Active
	p.simFailRate = sim.FailureRate
	p.simForceError = sim.ForceError

	// When simulation is cleared, reset health window to allow recovery
	if wasActive && !sim.Active {
		p.recentResults = p.recentResults[:0]
		p.approvalRate = 1.0
		p.status = StatusHealthy
	}
}

func (p *Processor) GetSimulation() SimulateRequest {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return SimulateRequest{
		FailureRate: p.simFailRate,
		ForceError:  p.simForceError,
		Active:      p.simActive,
	}
}

func (p *Processor) SetOverride(status ProcessorStatus) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.overrideActive = true
	p.overrideStatus = status
}

func (p *Processor) ClearOverride() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.overrideActive = false
	// Recalculate from window
	p.updateHealth()
}

func (p *Processor) Info() ProcessorInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return ProcessorInfo{
		ID:              p.ID,
		Priority:        p.Priority,
		CostPerTxn:      p.CostPerTxn,
		BaseSuccessRate: p.BaseSuccessRate,
		AvgLatencyMs:    p.AvgLatencyMs,
		GeoPreferences:  p.GeoPreferences,
		Status:          p.GetStatusLocked(),
		ApprovalRate:    p.approvalRate,
		TotalTxns:       p.totalTxns,
		TotalApproved:   p.totalApproved,
		TotalDeclined:   p.totalDeclined,
		TotalErrors:     p.totalErrors,
		CircuitState:    p.breaker.State().String(),
		Simulation: SimulateRequest{
			FailureRate: p.simFailRate,
			ForceError:  p.simForceError,
			Active:      p.simActive,
		},
	}
}

func (p *Processor) GetStatusLocked() ProcessorStatus {
	if p.overrideActive {
		return p.overrideStatus
	}
	return p.status
}

func (p *Processor) HasGeoPreference(country string) bool {
	for _, g := range p.GeoPreferences {
		if g == country {
			return true
		}
	}
	return false
}

// --- Router ---

const maxTransactions = 200

type Router struct {
	processors   []*Processor
	transactions []Transaction
	txnMu        sync.RWMutex
	txnCounter   int64
}

func NewRouter() *Router {
	return &Router{
		processors: []*Processor{
			NewProcessor("processor-a", 1, 0.25, 0.95, 50, []string{"BR", "CO"}),
			NewProcessor("processor-b", 2, 0.18, 0.85, 150, []string{"MX", "CL"}),
			NewProcessor("processor-c", 3, 0.10, 0.80, 300, []string{"BR", "MX", "CO", "CL"}),
		},
		transactions: make([]Transaction, 0, maxTransactions),
	}
}

type candidate struct {
	proc     *Processor
	geoScore int
}

func (r *Router) Route(ctx context.Context, req PaymentRequest) (PaymentResponse, error) {
	flow := make([]FlowStep, 0, 5)
	startTime := time.Now()

	// Step 1: Incoming Request
	flow = append(flow, FlowStep{
		Step: 1, Label: "Merchant Request", Direction: "inbound",
		Timestamp: startTime,
		Data: map[string]interface{}{
			"method":         "POST",
			"endpoint":       "/v1/payments/authorize",
			"amount":         req.Amount,
			"currency":       req.Currency,
			"payment_method": req.PaymentMethod,
			"country":        req.Country,
			"customer_id":    req.CustomerID,
		},
	})

	// Step 2: Routing Evaluation
	evaluated := make([]string, 0, len(r.processors))
	candidates := make([]candidate, 0, len(r.processors))
	evaluations := make([]ProcessorEvaluation, 0, len(r.processors))

	for _, p := range r.processors {
		evaluated = append(evaluated, p.ID)
		status := p.GetStatus()
		circuitState := p.breaker.State()
		geoMatch := p.HasGeoPreference(req.Country)

		eval := ProcessorEvaluation{
			ProcessorID:  p.ID,
			Status:       status,
			CircuitState: circuitState.String(),
			GeoMatch:     geoMatch,
			CostPerTxn:   p.CostPerTxn,
			Priority:     p.Priority,
			Eligible:     true,
		}

		if status == StatusDown {
			eval.Eligible = false
			eval.SkipReason = "processor_down"
			evaluations = append(evaluations, eval)
			continue
		}

		if circuitState == gobreaker.StateOpen {
			eval.Eligible = false
			eval.SkipReason = "circuit_open"
			evaluations = append(evaluations, eval)
			continue
		}

		evaluations = append(evaluations, eval)

		geoScore := 0
		if geoMatch {
			geoScore = 1
		}
		candidates = append(candidates, candidate{proc: p, geoScore: geoScore})
	}

	if len(candidates) == 0 {
		return PaymentResponse{}, fmt.Errorf("no_processors_available")
	}

	// Sort: HEALTHY > DEGRADED, then geo match, then priority, then cost
	sort.SliceStable(candidates, func(i, j int) bool {
		ci, cj := candidates[i], candidates[j]
		si := healthScore(ci.proc.GetStatus())
		sj := healthScore(cj.proc.GetStatus())
		if si != sj {
			return si > sj
		}
		if ci.geoScore != cj.geoScore {
			return ci.geoScore > cj.geoScore
		}
		if ci.proc.Priority != cj.proc.Priority {
			return ci.proc.Priority < cj.proc.Priority
		}
		return ci.proc.CostPerTxn < cj.proc.CostPerTxn
	})

	selected := candidates[0]
	routingTime := time.Now()

	flow = append(flow, FlowStep{
		Step: 2, Label: "Routing Evaluation", Direction: "internal",
		Timestamp:  routingTime,
		DurationMs: routingTime.Sub(startTime).Milliseconds(),
		Data: map[string]interface{}{
			"evaluations":        evaluations,
			"selected_processor": selected.proc.ID,
			"sort_order":         "health > geo > priority > cost",
		},
	})

	// Determine if this is a fallback
	primaryAvailable := false
	for _, c := range candidates {
		if c.proc.Priority == 1 && c.proc.GetStatus() == StatusHealthy {
			primaryAvailable = true
			break
		}
	}
	fallbackUsed := !primaryAvailable || selected.proc.Priority != 1

	// Step 3: Outbound Request to Processor
	procReqTime := time.Now()
	flow = append(flow, FlowStep{
		Step: 3, Label: "Processor Request", Direction: "outbound",
		Timestamp: procReqTime,
		Data: map[string]interface{}{
			"processor":    selected.proc.ID,
			"endpoint":     fmt.Sprintf("POST https://%s.payments.com/v1/authorize", selected.proc.ID),
			"amount":       req.Amount,
			"currency":     req.Currency,
			"payment_method": req.PaymentMethod,
		},
	})

	// Execute through circuit breaker
	result, err := selected.proc.breaker.Execute(func() (interface{}, error) {
		approved, latMs, authErr := selected.proc.Authorize(ctx, req)
		return [2]interface{}{approved, latMs}, authErr
	})

	now := time.Now()
	r.txnMu.Lock()
	r.txnCounter++
	txnID := fmt.Sprintf("txn_%06d", r.txnCounter)
	r.txnMu.Unlock()

	var status TransactionStatus
	var latencyMs int64
	var reason string

	if err != nil {
		status = TxnError
		selected.proc.RecordResult(false, true)
		reason = buildReason(selected, fallbackUsed, true)

		// Step 4: Processor Response (error)
		flow = append(flow, FlowStep{
			Step: 4, Label: "Processor Response", Direction: "response",
			Timestamp: now, DurationMs: now.Sub(procReqTime).Milliseconds(),
			Data: map[string]interface{}{
				"processor": selected.proc.ID,
				"status":    "ERROR",
				"error":     err.Error(),
			},
		})

		resp := PaymentResponse{
			TransactionID: txnID, Status: status,
			ProcessorUsed: selected.proc.ID,
			RoutingDecision: RoutingDecision{
				Reason: reason, ProcessorsEvaluated: evaluated,
				FallbackUsed: fallbackUsed, GeoMatch: selected.geoScore == 1,
				CostBasisUSD: selected.proc.CostPerTxn,
			},
			Amount: req.Amount, Currency: req.Currency,
			ProcessorMsg: err.Error(), CreatedAt: now,
		}

		// Step 5: Final Response
		flow = append(flow, FlowStep{
			Step: 5, Label: "Final Response", Direction: "response",
			Timestamp:  now,
			DurationMs: now.Sub(startTime).Milliseconds(),
			Data: map[string]interface{}{
				"transaction_id": txnID,
				"status":         status,
				"processor_used": selected.proc.ID,
				"routing_reason": reason,
				"total_time_ms":  now.Sub(startTime).Milliseconds(),
			},
		})
		resp.Flow = flow

		r.storeTransaction(Transaction{
			ID: txnID, ProcessorUsed: selected.proc.ID,
			Status: status, Amount: req.Amount, Currency: req.Currency,
			Country: req.Country, RoutingReason: reason, LatencyMs: 0,
			CreatedAt: now,
		})

		return resp, nil
	}

	pair := result.([2]interface{})
	approved := pair[0].(bool)
	latencyMs = pair[1].(int64)

	selected.proc.RecordResult(approved, false)

	if approved {
		status = TxnApproved
	} else {
		status = TxnDeclined
	}

	reason = buildReason(selected, fallbackUsed, false)

	// Step 4: Processor Response (success/decline)
	flow = append(flow, FlowStep{
		Step: 4, Label: "Processor Response", Direction: "response",
		Timestamp: now, DurationMs: latencyMs,
		Data: map[string]interface{}{
			"processor":  selected.proc.ID,
			"status":     status,
			"latency_ms": latencyMs,
			"reference":  fmt.Sprintf("%s-ref-%s", selected.proc.ID, txnID),
		},
	})

	resp := PaymentResponse{
		TransactionID: txnID, Status: status,
		ProcessorUsed: selected.proc.ID,
		RoutingDecision: RoutingDecision{
			Reason: reason, ProcessorsEvaluated: evaluated,
			FallbackUsed: fallbackUsed, GeoMatch: selected.geoScore == 1,
			CostBasisUSD: selected.proc.CostPerTxn,
		},
		Amount: req.Amount, Currency: req.Currency,
		CreatedAt: now,
	}

	// Step 5: Final Response
	flow = append(flow, FlowStep{
		Step: 5, Label: "Final Response", Direction: "response",
		Timestamp:  now,
		DurationMs: now.Sub(startTime).Milliseconds(),
		Data: map[string]interface{}{
			"transaction_id": txnID,
			"status":         status,
			"processor_used": selected.proc.ID,
			"routing_reason": reason,
			"total_time_ms":  now.Sub(startTime).Milliseconds(),
		},
	})
	resp.Flow = flow

	r.storeTransaction(Transaction{
		ID: txnID, ProcessorUsed: selected.proc.ID,
		Status: status, Amount: req.Amount, Currency: req.Currency,
		Country: req.Country, RoutingReason: reason, LatencyMs: latencyMs,
		CreatedAt: now,
	})

	return resp, nil
}

func (r *Router) storeTransaction(txn Transaction) {
	r.txnMu.Lock()
	defer r.txnMu.Unlock()
	if len(r.transactions) >= maxTransactions {
		r.transactions = r.transactions[1:]
	}
	r.transactions = append(r.transactions, txn)
}

func (r *Router) GetProcessors() []ProcessorInfo {
	infos := make([]ProcessorInfo, len(r.processors))
	for i, p := range r.processors {
		infos[i] = p.Info()
	}
	return infos
}

func (r *Router) GetTransactions(limit int) []Transaction {
	r.txnMu.RLock()
	defer r.txnMu.RUnlock()

	n := len(r.transactions)
	if limit <= 0 || limit > n {
		limit = n
	}

	// Return most recent first
	result := make([]Transaction, limit)
	for i := 0; i < limit; i++ {
		result[i] = r.transactions[n-1-i]
	}
	return result
}

func (r *Router) GetProcessor(id string) *Processor {
	for _, p := range r.processors {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// --- Helpers ---

func healthScore(s ProcessorStatus) int {
	switch s {
	case StatusHealthy:
		return 2
	case StatusDegraded:
		return 1
	default:
		return 0
	}
}

func buildReason(c candidate, fallback bool, isError bool) string {
	if isError {
		if fallback {
			return "fallback_processor_error"
		}
		return "primary_processor_error"
	}

	if !fallback {
		if c.geoScore == 1 {
			return "primary_healthy_geo_match"
		}
		return "primary_healthy"
	}

	// Fallback scenarios
	status := c.proc.GetStatus()
	geo := ""
	if c.geoScore == 1 {
		geo = "_geo_match"
	}

	switch status {
	case StatusDegraded:
		return "failover_to_degraded" + geo
	case StatusDown:
		return "last_resort_all_degraded"
	default:
		return "failover_primary_unavailable" + geo
	}
}
