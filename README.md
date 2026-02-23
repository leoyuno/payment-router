# Payment Router API - Zenith Gaming

Intelligent payment routing with automatic failover, built as a prototype for Zenith Gaming's microtransaction platform. The service selects the best available payment processor in real time based on health, geography, priority, and cost — with circuit breakers to prevent cascading failures.

---

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                          HTTP Layer                             │
│                   (chi router · :8080)                          │
└───────────────────────────┬─────────────────────────────────────┘
                            │
                            ▼
┌─────────────────────────────────────────────────────────────────┐
│                      Routing Engine                             │
│                                                                 │
│   Sort candidates:  Health → Geography → Priority → Cost        │
│   Select best → execute via circuit breaker → record outcome    │
└──────────┬──────────────────────────────────────────────────────┘
           │
           ├──────────────────────────────────┐
           ▼                                  ▼
┌──────────────────┐              ┌───────────────────────────────┐
│  Mock Processors │              │     Observability Layer        │
│  A · B · C       │              │                               │
└──────────────────┘              │  ┌─────────────────────────┐  │
                                  │  │    Health Monitor        │  │
                                  │  │  Sliding window (n=20)  │  │
                                  │  │  HEALTHY/DEGRADED/DOWN  │  │
                                  │  └─────────────────────────┘  │
                                  │                               │
                                  │  ┌─────────────────────────┐  │
                                  │  │    Circuit Breaker       │  │
                                  │  │  (gobreaker per proc)   │  │
                                  │  │  Tracks infra errors     │  │
                                  │  └─────────────────────────┘  │
                                  └───────────────────────────────┘
```

When a payment request arrives, the router evaluates all three processors across four dimensions (health status, geographic preference, configured priority, and cost per transaction), sorts them into a ranked candidate list, and dispatches the payment to the highest-ranked processor through its circuit breaker. The outcome (approved, declined, or error) is recorded in both the health monitor's sliding window and the transaction log, which updates the processor's health state for the next request.

**Health Monitor** tracks approval rates over the last 20 transactions and classifies each processor as HEALTHY, DEGRADED, or DOWN. It feeds directly into routing candidate selection.

**Circuit Breaker** (one per processor, via `gobreaker`) tracks connection-level errors — timeouts, unreachable endpoints — independently from business declines. After 5 consecutive infrastructure failures the breaker trips open for 10 seconds, then enters half-open state allowing 3 probe requests. A tripped breaker removes the processor from the candidate list entirely.

---

## Quick Start

```bash
cd payment-router
go run .
# Open http://localhost:8080 for the dashboard
```

No environment variables or external dependencies are required. All processors are mocked in memory.

---

## API Reference

| Method | Path | Description |
|--------|------|-------------|
| GET | `/` | Dashboard UI |
| POST | `/v1/payments/authorize` | Submit a payment for routing |
| GET | `/v1/processors` | List all processors with live health status |
| GET | `/v1/transactions` | Recent transaction log |
| POST | `/v1/admin/processors/{id}/simulate` | Set failure simulation on a processor |
| POST | `/v1/admin/processors/{id}/status` | Manual override a processor's status |

---

## Processor Configuration

| Processor | Role | Success Rate | Fee | Latency | Supported Regions |
|-----------|------|-------------|-----|---------|-------------------|
| A | Primary | 95% | $0.25 / txn | 50 ms | BR, CO |
| B | Secondary | 85% | $0.18 / txn | 150 ms | MX, CL |
| C | Backup | 80% | $0.10 / txn | 300 ms | All LATAM |

Processor C is the catch-all fallback for any country not explicitly preferred by A or B.

---

## Routing Algorithm

Candidates are filtered and sorted in this strict order:

1. **Health** — only HEALTHY or DEGRADED processors are eligible; DOWN processors and tripped circuit breakers are excluded entirely
2. **Geography** — processors that explicitly support the payment's country code are ranked above those that do not
3. **Priority** — processor configuration priority (1 = primary, 2 = secondary, 3 = backup)
4. **Cost** — among equally ranked candidates, the cheapest fee wins

Every payment response includes a `routing_decision` object explaining why the processor was chosen, whether a fallback was used, and the cost basis. A 5-step `flow` trace captures the full pipeline from inbound request to final response.

---

## API Response Examples

### POST /v1/payments/authorize

```json
{
  "transaction_id": "txn_000001",
  "status": "APPROVED",
  "processor_used": "processor-a",
  "routing_decision": {
    "reason": "primary_healthy_geo_match",
    "processors_evaluated": ["processor-a", "processor-b", "processor-c"],
    "fallback_used": false,
    "geo_match": true,
    "cost_basis_usd": 0.25
  },
  "amount": 1500,
  "currency": "BRL",
  "created_at": "2025-01-15T20:59:31Z",
  "flow": [
    {"step": 1, "label": "Merchant Request", "direction": "inbound", "...": "..."},
    {"step": 2, "label": "Routing Evaluation", "direction": "internal", "...": "..."},
    {"step": 3, "label": "Processor Request", "direction": "outbound", "...": "..."},
    {"step": 4, "label": "Processor Response", "direction": "response", "duration_ms": 47},
    {"step": 5, "label": "Final Response", "direction": "response", "duration_ms": 48}
  ]
}
```

Routing reason values: `primary_healthy_geo_match`, `primary_healthy`, `failover_primary_unavailable_geo_match`, `failover_to_degraded`, `last_resort_all_degraded`, `primary_processor_error`, `fallback_processor_error`.

### GET /v1/processors

Returns per-processor metrics including live health status, approval rate, transaction counts, and circuit breaker state:

```json
[
  {
    "id": "processor-a",
    "priority": 1,
    "cost_per_txn": 0.25,
    "base_success_rate": 0.95,
    "avg_latency_ms": 50,
    "geo_preferences": ["BR", "CO"],
    "status": "HEALTHY",
    "approval_rate": 0.95,
    "total_txns": 20,
    "total_approved": 19,
    "total_declined": 1,
    "total_errors": 0,
    "circuit_state": "closed",
    "simulation": {"failure_rate": 0, "force_error": false, "active": false}
  }
]
```

### GET /v1/transactions

Returns the most recent transactions (default 50, configurable via `?limit=N`):

```json
[
  {
    "id": "txn_000001",
    "processor_used": "processor-a",
    "status": "APPROVED",
    "amount": 1500,
    "currency": "BRL",
    "country": "BR",
    "routing_reason": "primary_healthy_geo_match",
    "latency_ms": 47,
    "created_at": "2025-01-15T20:59:31Z"
  }
]
```

### Error Responses

All errors return a structured JSON body with an `error` code and human-readable `message`:

```json
{"error": "validation_error", "message": "amount must be positive"}
{"error": "invalid_request", "message": "Invalid JSON body"}
{"error": "not_found", "message": "processor not found: xyz"}
{"error": "routing_failed", "message": "no_processors_available"}
```

---

## Health Monitoring

Each processor maintains a sliding window of its last **20 transactions**.

| State | Condition | Effect |
|-------|-----------|--------|
| HEALTHY | Success rate >= 70% | Eligible for routing |
| DEGRADED | Success rate < 70% | Still eligible, ranked lower |
| DOWN | Success rate < 50% | Excluded from routing |

State transitions are evaluated after every transaction. Recovery is automatic — when a failure simulation is cleared, the health window resets and the processor immediately returns to HEALTHY, allowing it to reclaim its primary routing slot without waiting for the window to flush.

**Why these thresholds?** In payment processing, a 70% approval rate is already a serious problem (3 out of 10 customers are failing). The 50% threshold for DOWN is the point where routing traffic to this processor causes more harm than good. The 20-transaction window balances responsiveness (detecting problems quickly) against stability (not overreacting to a few random declines).

**Important distinction:** the Health Monitor tracks *approval rates* (business outcomes). The Circuit Breaker tracks *connection errors* (infrastructure outcomes). A processor can be HEALTHY but have its circuit breaker tripped if it starts timing out, and vice versa.

---

## Demo Walkthrough

Open two windows: a terminal, and a browser at `http://localhost:8080`. Follow these steps sequentially to see the full routing engine in action.

### Step 1 — Normal routing (BR → Processor A)

```bash
curl -s -X POST http://localhost:8080/v1/payments/authorize \
  -H "Content-Type: application/json" \
  -d '{"amount":1500,"currency":"BRL","payment_method":"CREDIT_CARD","country":"BR","customer_id":"demo_user"}'
```

**Expected:** `"processor_used": "processor-a"`, `"reason": "primary_healthy_geo_match"`. The dashboard shows all three processors HEALTHY.

### Step 2 — Geographic routing (MX → Processor B)

```bash
curl -s -X POST http://localhost:8080/v1/payments/authorize \
  -H "Content-Type: application/json" \
  -d '{"amount":500,"currency":"MXN","payment_method":"CREDIT_CARD","country":"MX","customer_id":"demo_mx"}'
```

**Expected:** `"processor_used": "processor-b"` with `"geo_match": true`. Processor B has geo preference for MX.

### Step 3 — Trigger failover

```bash
# Activate 100% failure simulation on Processor A
curl -s -X POST http://localhost:8080/v1/admin/processors/processor-a/simulate \
  -H "Content-Type: application/json" \
  -d '{"active":true,"failure_rate":1.0,"force_error":false}'

# Send 8 payments to fill the health window — watch the failover happen
for i in $(seq 1 8); do
  curl -s -X POST http://localhost:8080/v1/payments/authorize \
    -H "Content-Type: application/json" \
    -d '{"amount":100,"currency":"BRL","payment_method":"CREDIT_CARD","country":"BR","customer_id":"failover_test"}' \
    | python3 -c "import json,sys; r=json.load(sys.stdin); print(f'{r[\"processor_used\"]}: {r[\"status\"]} ({r[\"routing_decision\"][\"reason\"]})')"
done
```

**Expected:** First ~4 payments go to Processor A (DECLINED). Once A's approval rate drops below 50%, it becomes DOWN and subsequent payments failover to Processor C with reason `failover_primary_unavailable_geo_match`. Check the dashboard — Processor A's card turns red showing DOWN status.

### Step 4 — Recovery

```bash
# Clear the failure simulation
curl -s -X POST http://localhost:8080/v1/admin/processors/processor-a/simulate \
  -H "Content-Type: application/json" \
  -d '{"active":false,"failure_rate":0,"force_error":false}'

# Verify A recovered immediately
curl -s http://localhost:8080/v1/processors | python3 -c "
import json,sys
for p in json.load(sys.stdin):
    print(f'{p[\"id\"]}: {p[\"status\"]}')"

# Send a BR payment — A should be primary again
curl -s -X POST http://localhost:8080/v1/payments/authorize \
  -H "Content-Type: application/json" \
  -d '{"amount":1500,"currency":"BRL","payment_method":"CREDIT_CARD","country":"BR","customer_id":"recovery_test"}' \
  | python3 -c "import json,sys; r=json.load(sys.stdin); print(f'{r[\"processor_used\"]}: {r[\"routing_decision\"][\"reason\"]}')"
```

**Expected:** Processor A returns to HEALTHY immediately (health window resets on simulation clear). The next BR payment routes back to `processor-a` with reason `primary_healthy_geo_match`.

### Step 5 — Manual override

```bash
# Force Processor A offline via admin override
curl -s -X POST http://localhost:8080/v1/admin/processors/processor-a/status \
  -H "Content-Type: application/json" \
  -d '{"status":"DOWN","reason":"maintenance window"}'

# BR payment now skips A entirely
curl -s -X POST http://localhost:8080/v1/payments/authorize \
  -H "Content-Type: application/json" \
  -d '{"amount":1500,"currency":"BRL","payment_method":"CREDIT_CARD","country":"BR","customer_id":"override_test"}' \
  | python3 -c "import json,sys; r=json.load(sys.stdin); print(f'{r[\"processor_used\"]}: fallback={r[\"routing_decision\"][\"fallback_used\"]}')"

# Restore A
curl -s -X POST http://localhost:8080/v1/admin/processors/processor-a/status \
  -H "Content-Type: application/json" \
  -d '{"status":"HEALTHY"}'
```

**Expected:** While overridden to DOWN, Processor A is excluded from routing. BR payments go to Processor C. After clearing the override, A reclaims the primary slot.

---

## Technical Decisions

### Why Go
Payment systems demand low latency and high concurrency. Go's goroutine model handles hundreds of simultaneous routing decisions with minimal overhead, and its standard library ships everything needed for HTTP and JSON without pulling in a large framework. It is also the industry-standard language for payment infrastructure at companies like Stripe, Adyen, and Square.

### Why in-memory state
This is a prototype with no persistence requirement. In-memory maps protected by `sync.RWMutex` are fast, simple, and sufficient for demonstrating routing logic without introducing a database dependency that would complicate setup for reviewers.

### Why chi
Chi is a lightweight, idiomatic HTTP router that is 100% compatible with `net/http` handlers. It adds URL parameter parsing and middleware chaining without imposing an opinionated framework structure — making it easy to swap out or extend later.

### Why gobreaker
`gobreaker` is the most widely adopted circuit breaker library in the Go ecosystem. It implements the standard three-state machine (Closed → Open → Half-Open) with configurable thresholds. Using a well-known library here signals production intent; implementing a custom breaker would have been over-engineering for a prototype.

### Health Monitor vs Circuit Breaker separation
These two mechanisms track fundamentally different failure modes:

- **Health Monitor**: "Is this processor approving payments?" — tracks business outcomes (approvals vs declines). A processor can be technically reachable but approving only 40% of transactions due to issuer rules, risk policies, or quota limits.
- **Circuit Breaker**: "Can we even reach this processor?" — tracks infrastructure errors (connection refused, timeout, 5xx). A tripped breaker prevents the router from wasting latency budget on a processor that is unreachable.

Separating them allows routing decisions to be precise: you can route around a degraded processor (high decline rate) without also tripping infrastructure protection, and vice versa.

---

## Stretch Goals Implemented

The following features went beyond the core routing requirement and were added to demonstrate production-readiness thinking:

| Feature | Description |
|---------|-------------|
| Circuit Breaker | `gobreaker` instance per processor; tracks connection errors separately from business declines |
| Cost-Aware Routing | Cheapest healthy processor is preferred after health and geography scoring |
| Geographic Routing | Processors declare country preferences; payments are matched to preferred processors before falling back |
| Manual Override Controls | Admin endpoints allow operators to force a processor's status independently of health metrics |

---

## What I'd Improve with More Time

- **Persistent storage** — Replace in-memory maps with PostgreSQL for transaction history and processor configuration. The current design would need a thin repository layer added between the routing service and the data layer.
- **Real processor integrations** — Swap mock processors for actual HTTP clients calling Stripe, dLocal, or Adyen sandbox environments with proper authentication, retry headers, and idempotency keys.
- **Weighted routing / traffic splitting** — Allow traffic to be split across processors by percentage (e.g., 70% A / 30% B) for A/B testing new processors or gradual rollouts.
- **Anti-flapping on recovery** — Require a processor to sustain a healthy window for at least 10 seconds before being re-admitted to primary routing, preventing oscillation between HEALTHY and DOWN.
- **Prometheus metrics** — Expose `/metrics` with labeled counters for attempt counts, success rates, latency histograms, and failover events per processor.
- **WebSocket for real-time dashboard** — Replace the dashboard's polling model with a WebSocket feed so operators see health and transaction events as they happen without refreshing.
