# Manual Test Scenarios

6 controlled scenarios demonstrating the order processing system to a reviewer.
Each step is published with a 3s pause so state changes can be watched live.

---

## Prerequisites

```powershell
# 1. Start infrastructure
docker compose up -d

# 2. Start Go service (separate terminal)
go run main.go

# 3. (Optional) Start dashboard
cd dashboard
pip install -r requirements.txt
streamlit run app.py
```

## Database Reset (run between test sessions)

```powershell
docker exec -it food_delivery_db psql -U vaurd_user -d food_delivery -c "TRUNCATE TABLE orders;"
```

## Run the Simulation

```powershell
python generator/manual_simulation.py
```

---

## Scenario 1: Happy Path — Normal Order Lifecycle

**Order:** Order-A (Alice orders 2x Margherita Pizza + 1x Coke from Pizzeria)  
**Goal:** Demonstrate `Received → Preparing → Complete`.

| Step | Action | Expected State | Verification |
|------|--------|---------------|--------------|
| 1 | `order.create` (customer-alice, pizzeria, margherita x2, coke x1) | `status: "Received"`, items correct | `curl -s http://localhost:8080/orders?limit=10 \| python -m json.tool` |
| 2 | `order.update.status` → `Preparing` | `status: "Preparing"` | `curl -s "http://localhost:8080/orders?status=Preparing"` |
| 3 | `order.update.status` → `Complete` | `status: "Complete"` | `curl -s "http://localhost:8080/orders?status=Complete"` |

**Demo script:** The lifecycle works end-to-end. Each status update correctly advances the order.

---

## Scenario 2: Cancellation from Non-Terminal State

**Order:** Order-B (Bob orders 1x Cheeseburger, then cancels)  
**Goal:** Show `Cancelled` is reachable from `Received`.

| Step | Action | Expected State | Verification |
|------|--------|---------------|--------------|
| 1 | `order.create` (customer-bob, burger-shop, cheeseburger x1) | `status: "Received"` | `curl -s http://localhost:8080/orders` |
| 2 | `order.update.status` → `Cancelled` | `status: "Cancelled"` | `curl -s "http://localhost:8080/orders?status=Cancelled"` |

**Demo script:** `Received → Cancelled` is valid. The state machine allows cancellation before preparation begins.

---

## Scenario 3: Items Replacement

**Order:** Order-C (Charlie orders 3x Salmon Roll, then changes to Ramen + Gyoza)  
**Goal:** Items are **replaced**, not merged.

| Step | Action | Expected Items | Verification |
|------|--------|---------------|--------------|
| 1 | `order.create` (customer-charlie, sushi-bar, salmon-roll x3) | `[{"itemId":"salmon-roll","qty":3}]` | `curl -s http://localhost:8080/orders` |
| 2 | `order.update.items` → ramen x1, gyoza x2 | `[{"itemId":"ramen","qty":1},{"itemId":"gyoza","qty":2}]` | Check `items` field in response |

**Demo script:** The JSONB field is **replaced** entirely. Old items are not merged with new ones.

---

## Scenario 4: Invalid Status Transition

**Order:** Order-D (Diana's salad goes Complete, then tries to go back to Preparing)  
**Goal:** State machine **rejects** `Complete → Preparing`.

| Step | Action | Expected Status | Verification |
|------|--------|----------------|--------------|
| 1 | `order.create` (customer-diana, salad-stop, caesar-salad x1) | `Received` | `curl -s http://localhost:8080/orders` |
| 2 | `order.update.status` → `Complete` | `Complete` | `curl -s "http://localhost:8080/orders?status=Complete"` |
| 3 | `order.update.status` → `Preparing` (INVALID) | **Still `Complete`** (rejected) | Check service terminal for log: `invalid status transition rejected: order=... from=Complete to=Preparing` |

**Demo script:** The state machine is enforced atomically in the UPSERT. Invalid transitions are logged and counted (`InvalidTransitionCount`) but do **not** crash or stop processing.

---

## Scenario 5: Update-Before-Create (Defensive Robustness)

**Order:** Order-E  
**Goal:** A status update arriving before any create creates a **partial row** without crashing.

| Step | Action | Expected State | Verification |
|------|--------|---------------|--------------|
| 1 | `order.update.status` → `Preparing` (no order exists yet) | Partial row: `status: "Preparing"`, `customerId: null`, `restaurantId: null` | `curl -s http://localhost:8080/orders` — look for null fields |
| 2 | `order.update.items` → taco x3 (fills items on the partial row) | Items now `taco x3`, status stays `Preparing` | Items update has `status=NULL` → bypasses state machine guard |
| 3 | `order.create` (customer-eve, taco-stand) | **No change** — gracefully handled as no-op | State machine blocks `Preparing → Received` |

**Demo script:** Three aspects of robustness in one order:
1. **Partial row:** An update for an unknown order creates a row instead of crashing.
2. **Items on partial row:** Items updates can modify partial rows since they carry no status.
3. **Late create:** The create arrives harmlessly — the system doesn't error or crash.

> **Design note:** The create cannot fill `customerId`/`restaurantId` nulls because the UPSERT combines LWW + state machine in one atomic condition; the create's status (`Received`) would be blocked by the state machine from overriding an existing `Preparing`. This is a deliberate trade-off — the state machine guard takes priority over null-filling. A production system could use a separate background reconciler for this edge case.

---

## Scenario 6: Duplicate Events + Stale Timestamps (LWW)

**Order:** Order-F (Frank orders 1x Pad Thai)  
**Goal:** Demonstrate idempotency (replayed create) and LWW guard (backdated update).

| Step | Action | Expected Result | Verification |
|------|--------|----------------|--------------|
| 1 | `order.create` (customer-frank, noodle-house, pad-thai x1) | `status: "Received"` | `curl -s http://localhost:8080/orders` |
| 2 | **Replay** exact same create (same `eventId`) | **No change** — PK conflict → no-op | Verify total order count is unchanged |
| 3 | `order.update.status` → `Preparing` | `status: "Preparing"` | `curl -s "http://localhost:8080/orders?status=Preparing"` |
| 4 | **Stale** `order.update.status` → `Complete` with `occurredAt: 2024-01-01T00:00:00Z` (year old) | **Still `Preparing`** — LWW rejects old timestamp | `curl -s "http://localhost:8080/orders?status=Complete"` returns empty |

**Demo script:**
- **Idempotency:** Deterministic UUIDv5 makes create replay safe — same `eventId` → same `orderId` → PK conflict → no-op.
- **LWW guard:** A backdated event (old `occurredAt`) is rejected because `EXCLUDED.last_event_at > orders.last_event_at` is false. Ordering is by **event time**, not arrival time.

---

## Final State Summary

After the full simulation completes, run:

```powershell
curl -s "http://localhost:8080/orders?limit=10" | python -m json.tool
```

| Order | Customer | Status | Items | Key Demonstration |
|-------|----------|--------|-------|-------------------|
| Order-A | customer-alice | `Complete` | margherita x2, coke x1 | Happy path lifecycle |
| Order-B | customer-bob | `Cancelled` | cheeseburger x1 | Cancellation from Received |
| Order-C | customer-charlie | `Received` | ramen x1, gyoza x2 | Items replaced via update.items |
| Order-D | customer-diana | `Complete` | caesar-salad x1 | Invalid Preparing transition rejected |
| Order-E | — | `Preparing` | taco x3 | Update-before-create partial row |
| Order-F | customer-frank | `Preparing` | pad-thai x1 | Duplicate replay + stale LWW rejection |

---

## Edge Case Quick Reference

| §5 Concern | Covered In | How to Demonstrate |
|-----------|-----------|-------------------|
| Out-of-order events | #5, #6 step 4 | Update-before-create (#5) + backdated timestamp (#6) |
| Concurrency | #1, #4 | Atomic UPSERT handles parallel events — run generator alongside |
| Duplicates / replay | #6 step 2 | Same create event (`eventId`) → no-op |
| Status transitions | #1, #2, #4 | Valid transitions (#1, #2); invalid rejected (#4) |
| Throughput | all | Run generator at high `EVENT_INTERVAL_SECONDS` — JetStream buffers |
| "Latest" guarantees | #6 step 4 | Backdated event rejected; current state reflects newest `occurredAt` |
