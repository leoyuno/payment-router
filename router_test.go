package main

import (
	"context"
	"strings"
	"testing"
)

// helper builds a standard payment request for a given country.
func makeReq(country, currency string) PaymentRequest {
	return PaymentRequest{
		Amount:        1000,
		Currency:      currency,
		PaymentMethod: "CREDIT_CARD",
		Country:       country,
		CustomerID:    "test_user",
	}
}

// TestRoutingSortOrder verifies that geographic preference and priority
// produce the expected processor selection for different countries.
func TestRoutingSortOrder(t *testing.T) {
	tests := []struct {
		name      string
		country   string
		currency  string
		wantProc  string
		wantGeo   bool
	}{
		{"BR routes to processor-a (priority 1, geo match)", "BR", "BRL", "processor-a", true},
		{"MX routes to processor-b (priority 2, geo match)", "MX", "MXN", "processor-b", true},
		{"CO routes to processor-a (priority 1, geo match)", "CO", "COP", "processor-a", true},
		{"CL routes to processor-b (priority 2, geo match)", "CL", "CLP", "processor-b", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := NewRouter()
			resp, err := router.Route(context.Background(), makeReq(tt.country, tt.currency))
			if err != nil {
				t.Fatalf("Route() error: %v", err)
			}
			if resp.ProcessorUsed != tt.wantProc {
				t.Errorf("ProcessorUsed = %q, want %q", resp.ProcessorUsed, tt.wantProc)
			}
			if resp.RoutingDecision.GeoMatch != tt.wantGeo {
				t.Errorf("GeoMatch = %v, want %v", resp.RoutingDecision.GeoMatch, tt.wantGeo)
			}
		})
	}
}

// TestHealthStateTransitions feeds declining results into a processor and
// verifies the HEALTHY → DEGRADED → DOWN state transitions.
func TestHealthStateTransitions(t *testing.T) {
	p := NewProcessor("test-proc", 1, 0.20, 0.95, 10, []string{"BR"})

	// Start HEALTHY
	if got := p.GetStatus(); got != StatusHealthy {
		t.Fatalf("initial status = %q, want HEALTHY", got)
	}

	// Feed 5 successes to fill minimum window
	for i := 0; i < 5; i++ {
		p.RecordResult(true, false)
	}
	if got := p.GetStatus(); got != StatusHealthy {
		t.Fatalf("after 5 successes: status = %q, want HEALTHY", got)
	}

	// Feed failures to bring approval rate below 70% → DEGRADED
	// Current window: 5 successes. Add 5 failures → 5/10 = 50% → DOWN
	// To hit DEGRADED first, add 3 failures → 5/8 = 62.5% → DEGRADED
	for i := 0; i < 3; i++ {
		p.RecordResult(false, false)
	}
	if got := p.GetStatus(); got != StatusDegraded {
		t.Errorf("after 3 failures (5/8=62.5%%): status = %q, want DEGRADED", got)
	}

	// Add more failures to bring below 50% → DOWN
	// Current: 5 true, 3 false = 8 results. Add 3 more false → 5/11 = 45% → DOWN
	for i := 0; i < 3; i++ {
		p.RecordResult(false, false)
	}
	if got := p.GetStatus(); got != StatusDown {
		t.Errorf("after 6 total failures (5/11=45%%): status = %q, want DOWN", got)
	}
}

// TestFailoverCascade forces an error on processor-a and verifies that the
// router cascades to processor-c for a BR payment.
func TestFailoverCascade(t *testing.T) {
	router := NewRouter()

	// Force infrastructure error on processor-a
	procA := router.GetProcessor("processor-a")
	procA.SetSimulation(SimulateRequest{Active: true, ForceError: true})

	resp, err := router.Route(context.Background(), makeReq("BR", "BRL"))
	if err != nil {
		t.Fatalf("Route() error: %v", err)
	}

	// processor-a should fail, cascade to processor-c (BR geo match, priority 3)
	if resp.ProcessorUsed == "processor-a" {
		t.Errorf("expected cascade away from processor-a, got processor-a")
	}
	if resp.RoutingDecision.Retries < 1 {
		t.Errorf("Retries = %d, want >= 1", resp.RoutingDecision.Retries)
	}
	if resp.Status == TxnError {
		t.Errorf("Status = ERROR, expected cascade to succeed on another processor")
	}
	if !strings.HasSuffix(resp.RoutingDecision.Reason, "_after_retry") {
		t.Errorf("Reason = %q, expected _after_retry suffix", resp.RoutingDecision.Reason)
	}
}

// TestRecoveryIsGradual clears a simulation and verifies the processor enters
// DEGRADED (not HEALTHY), then feeds successes to reach HEALTHY.
func TestRecoveryIsGradual(t *testing.T) {
	p := NewProcessor("test-proc", 1, 0.20, 0.95, 10, []string{"BR"})

	// Activate and then clear simulation
	p.SetSimulation(SimulateRequest{Active: true, FailureRate: 1.0})
	p.SetSimulation(SimulateRequest{Active: false})

	// Should be DEGRADED after clearing (seeded at 60%)
	if got := p.GetStatus(); got != StatusDegraded {
		t.Errorf("after clearing simulation: status = %q, want DEGRADED", got)
	}

	// Feed successes to recover to HEALTHY
	// Window: [true, true, true, false, false] = 60%. Need to push above 70%.
	// Adding 5 successes → window grows to 10: 8 true / 10 = 80% → HEALTHY
	for i := 0; i < 5; i++ {
		p.RecordResult(true, false)
	}
	if got := p.GetStatus(); got != StatusHealthy {
		t.Errorf("after 5 additional successes: status = %q, want HEALTHY", got)
	}
}

// TestGeographicRouting verifies that each LATAM country routes to its
// geographically preferred processor.
func TestGeographicRouting(t *testing.T) {
	tests := []struct {
		country  string
		wantProc string
	}{
		{"BR", "processor-a"},
		{"CO", "processor-a"},
		{"MX", "processor-b"},
		{"CL", "processor-b"},
	}

	for _, tt := range tests {
		t.Run(tt.country, func(t *testing.T) {
			router := NewRouter()
			resp, err := router.Route(context.Background(), makeReq(tt.country, "USD"))
			if err != nil {
				t.Fatalf("Route() error: %v", err)
			}
			if resp.ProcessorUsed != tt.wantProc {
				t.Errorf("country %s: ProcessorUsed = %q, want %q",
					tt.country, resp.ProcessorUsed, tt.wantProc)
			}
			if !resp.RoutingDecision.GeoMatch {
				t.Errorf("country %s: expected GeoMatch=true", tt.country)
			}
		})
	}
}

// TestAllProcessorsDown verifies that when all processors are DOWN, the
// router returns a no_processors_available error.
func TestAllProcessorsDown(t *testing.T) {
	router := NewRouter()

	// Override all processors to DOWN
	for _, id := range []string{"processor-a", "processor-b", "processor-c"} {
		router.GetProcessor(id).SetOverride(StatusDown)
	}

	_, err := router.Route(context.Background(), makeReq("BR", "BRL"))
	if err == nil {
		t.Fatal("expected error when all processors are DOWN, got nil")
	}
	if err.Error() != "no_processors_available" {
		t.Errorf("error = %q, want %q", err.Error(), "no_processors_available")
	}
}

// TestManualOverride verifies that a manual override to DOWN excludes the
// processor, and clearing the override restores it.
func TestManualOverride(t *testing.T) {
	router := NewRouter()
	procA := router.GetProcessor("processor-a")

	// Override processor-a to DOWN
	procA.SetOverride(StatusDown)

	resp, err := router.Route(context.Background(), makeReq("BR", "BRL"))
	if err != nil {
		t.Fatalf("Route() error: %v", err)
	}
	if resp.ProcessorUsed == "processor-a" {
		t.Error("processor-a should be excluded when overridden to DOWN")
	}

	// Clear override — processor-a should be eligible again
	procA.ClearOverride()
	if got := procA.GetStatus(); got == StatusDown {
		t.Errorf("after clearing override: status = %q, should not be DOWN", got)
	}

	resp, err = router.Route(context.Background(), makeReq("BR", "BRL"))
	if err != nil {
		t.Fatalf("Route() error after clearing override: %v", err)
	}
	if resp.ProcessorUsed != "processor-a" {
		t.Errorf("after clearing override: ProcessorUsed = %q, want processor-a", resp.ProcessorUsed)
	}
}

// TestCostTiebreaker verifies that when two processors have equal health,
// geography, and priority, the cheaper one wins.
func TestCostTiebreaker(t *testing.T) {
	// Create a custom router with two processors that differ only in cost
	r := &Router{
		processors: []*Processor{
			NewProcessor("expensive", 1, 0.50, 0.95, 50, []string{"BR"}),
			NewProcessor("cheap", 1, 0.10, 0.95, 50, []string{"BR"}),
		},
		transactions: make([]Transaction, 0, maxTransactions),
	}

	resp, err := r.Route(context.Background(), makeReq("BR", "BRL"))
	if err != nil {
		t.Fatalf("Route() error: %v", err)
	}
	if resp.ProcessorUsed != "cheap" {
		t.Errorf("ProcessorUsed = %q, want %q (cheaper processor)", resp.ProcessorUsed, "cheap")
	}
	if resp.RoutingDecision.CostBasisUSD != 0.10 {
		t.Errorf("CostBasisUSD = %.2f, want 0.10", resp.RoutingDecision.CostBasisUSD)
	}
}
