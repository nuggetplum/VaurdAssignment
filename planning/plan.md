# Vaurd Backend Assignment — Working Plan (DRAFT for discussion)

> Status: **planning complete — all decisions locked (§2). No pipeline code written yet.**
> This file supersedes `geminiplan.md` as the source of truth.
> Author: Aditya K. Last revised during planning session 2026-07-19.
>
> Final stack: Python generator → **NATS JetStream** → Go consumer → **PostgreSQL**;
> **HTTP/JSON** List API. Correctness via **event-time LWW conditional UPSERT** +
> enforced status machine. Idempotency via `event_id` + LWW no-op.

---

## 0. What the assignment actually rewards (read this first)

Priority order from §8 of the brief:
1. **Correctness** — accurate, latest state under a continuous, chaotic stream.
2. **Design & reasoning** — quality of trade-offs + a clear DESIGN.md answering every §5 edge case.
3. **Go fundamentals** — idiomatic, concurrency, error handling, structure.
4. **Runnability** — clone → `docker-compose up` → works, with clear docs.
5. **API quality** — sensible, documented, correct.

Guiding quote: *"A simpler solution you fully understand beats a complex one you can't explain."*
Every choice below is optimized for **being able to explain it in depth in the interview.**

---

## 1. Architecture (target)

```
┌───────────┐  publish  ┌──────────┐  consume  ┌──────────────────────────┐  HTTP/JSON  ┌──────────┐
│ Generator │ ────────▶ │  BROKER  │ ────────▶ │      Go Backend Service   │ ◀────────── │ Reviewer │
│ (Python)  │  events   │ (stream) │  group    │ consume→order→persist→ack │   GET /orders│  curl    │
└───────────┘           └──────────┘           └──────────────────────────┘             └──────────┘
      ▲ discovers orderIds via GET /orders                    │
      └──────────────────────────────────────────────┐       ▼
                                                      │  ┌─────────────┐
                                                      └──│  Postgres   │ (current state, LWW by event time)
                                                         └─────────────┘
```

Fixed decisions (see §2 for full rationale):
- **Language:** Go backend (mandatory), Python generator.
- **Transport:** NATS JetStream durable stream `ORDERS`, durable consumer + ack → at-least-once.
- **Query API:** HTTP/JSON with pagination + status filter + sort-by-updated.
- **DB:** PostgreSQL + pgxpool + golang-migrate (go:embed). Already in place.
- **State model:** one row per order = current state. Ordering by **event time**, not arrival time.

---

## 2. DECISIONS

### ✅ LOCKED (resolved 2026-07-19)
- **D1. Transport = real broker.** Generator publishes to a durable stream; Go service is a
  **consumer**. Best fit for the "event-driven runtime-state" theme; gives durability,
  consumer groups, at-least-once, and replay — real mechanisms to point at in DESIGN.md.
  → gRPC/protobuf is **dropped entirely** (ingestion is via broker, query is HTTP).
- **D2. List Orders API = HTTP/JSON.** curl-able; trivial pagination/filter/sort; best runnability.
- **D3. Dashboard = cut** from core scope. Build last only if time remains.
- **D4. Status transitions = enforced, not dropped.**
  `Received → Preparing → Complete`; `Cancelled` from any non-terminal.
  Invalid transitions are logged + counted, last valid state kept. Justify in DESIGN.

- **D5. Broker = NATS JetStream.** Durable stream `ORDERS`, a durable consumer, explicit
  `Ack()/Nak()/Term()`. Purpose-built for streaming with a first-class Go client; the
  stream + durable-consumer + ack model maps 1:1 to the §5 edge cases.
- **D6. Generator learns orderIds by discovery — with a throttled local cache.**
  Generator maintains a **local cache** of known orderIds and refreshes it from `GET /orders`
  on a low cadence (every few seconds, and/or ~10% of loop iterations) — NOT on every event.
  Updates are drawn from the cache; creates carry no id (service derives it).
  - Respects the system boundary ("service generates the orderId") while avoiding hot polling.
  - DESIGN.md note: a production system would instead emit `order.created` back to the
    generator (feedback stream) rather than poll the query API.
- **D7. orderId is deterministic: `UUIDv5(ORDER_NAMESPACE, create.event_id)`** (not random v4).
  Makes create replays idempotent via PK conflict — no dedup table. See §3.5–3.7.
- **D8. update-before-create = keep D6 + defensive merge + demo hook** (can't occur via the
  generator; implemented and proven anyway). See §3.7.

---

## 3. Core correctness design (the part that scores)

### 3.1 Event envelope (we design this)
```
Event {
  event_id     : string   // UUID — dedup key (idempotency)
  event_type   : enum     // order.create | order.update.status | order.update.items
  occurred_at  : timestamp// EVENT time — the ordering key (NOT arrival time)
  // + one of the type-specific payloads from §3
}
```
`occurred_at` (optionally plus a per-order sequence) is the linchpin for out-of-order + idempotency.

### 3.2 Last-Write-Wins conditional UPSERT
- `INSERT ... ON CONFLICT (order_id) DO UPDATE ... WHERE excluded.occurred_at > orders.last_event_at`
- Store `last_event_at` (event time) separately from `last_updated_at` (wall clock for the API's "last-updated").
- Effect: older/replayed events become **no-ops** → idempotent + out-of-order safe in one mechanism.

### 3.3 Out-of-order "update before create"
- `customer_id` / `restaurant_id` are nullable (already are in `models.Order`).
- An update arriving first inserts a partial row; the later create fills the nulls **without** clobbering a newer status (guarded by 3.2).

### 3.4 Concurrency
- **Correctness layer:** the atomic conditional UPSERT (DB row lock + version guard). Correct even with N concurrent writers.
- **Throughput layer (optional, defense-in-depth):** hashed worker pool — hash `order_id` → same worker → per-order serialization + bounded DB concurrency. Framed as an optimization, not the correctness guarantee.

### 3.5 Idempotency (split by event type — this distinction matters)
- **Updates** are idempotent for free: a replay carries the same `occurred_at`, which fails the
  strict `>` LWW guard (3.2) → no-op. No dedup table needed.
- **Creates need a stable id.** A random UUIDv4 assigned per-consume would mint a NEW order on
  every replay (the `order_id` PK conflict never fires) → duplicates. **Fix:** the service derives
  `order_id = UUIDv5(ORDER_NAMESPACE, create.event_id)`. A replayed create derives the *same*
  order_id → `ON CONFLICT (order_id) DO NOTHING` → genuine no-op. Service still "generates" the
  id (it computes it), so §3.1 holds, and we need **no separate dedup table**.

### 3.6 orderId + event contract (explicit)
- `order_id` is created **only** on `order.create`; the service derives it as
  **`UUIDv5(ORDER_NAMESPACE, create.event_id)`** (deterministic → replay-safe).
- `order.update.*` events **must** carry an `order_id` (the generator got it from `GET /orders`).
- Every event carries `event_id` (producer-generated) and `occurred_at` (the ordering key).

### 3.7 "update before create" — cannot occur via the generator; handled defensively anyway
In our discovery model (D6) the generator only learns an `order_id` from `GET /orders` *after* the
create is persisted, so an update can never be born before its create exists. Proof:
```
t0 generator publishes create (no id)
t1 service mints order_id=X, persists row X
t2 generator polls GET /orders, learns X
t3 generator publishes update(X)        // t3 > t1, so row X already exists
```
We still implement the defensive path (robustness + honest §5 answer):
- **update arrives first** → INSERT partial row keyed by its `order_id`; the update's own values
  set the initial baseline (customer/restaurant left NULL).
- **create arrives later** → fills NULL `customer_id`/`restaurant_id`; does **not** overwrite a
  newer status (LWW guard by `occurred_at`).
- **create arrives twice** → no-op via deterministic id + PK conflict (3.5).
- **update references unknown order_id** → treated as valid (partial insert), per above.
- **State-machine nuance (D4):** the *first* event for an order sets the status baseline directly;
  transition rules apply only from the *second* event onward (a partial row from an update-first
  has no "Received" baseline to transition from).
- **Demo/test hook:** publish a raw out-of-order update straight into JetStream (bypassing the
  generator's discovery) to prove the defensive merge works end-to-end.

---

## 4. Execution roadmap (revised from geminiplan)

Legend: ✅ done · ⬜ todo · 🔧 cleanup

### Phase 0 — Foundation (DONE) 
- ✅ Go module, Postgres compose, migrations, `models/order.go`.
- 🔧 **Cleanup task:** remove migration self-repair hack in `db/database.go` (L59–73) and `cmd/reset_migrations/`; do a clean migration reset. Fix schema: add `last_event_at`; clarify `last_updated_at`.

### Phase 1 — Contracts (event envelope + JSON API shapes)
- ⬜ Lock the event envelope (§3.1) as Go structs + JSON schema (broker payload = JSON).
- ⬜ Define HTTP request/response structs + route table for `GET /orders`.

### Phase 2 — Repository layer (`db/repository.go`)
- ⬜ `ApplyEvent(ctx, event)` = LWW conditional UPSERT (§3.2), JSONB items, null-merge (§3.3), status-machine guard (D4).
- ⬜ `ListOrders(ctx, filter/sort/paginate)` → ordered by `last_updated_at DESC`.

### Phase 3 — Broker consumer + concurrency
- ⬜ Consumer that reads from the durable stream via a **consumer group** and **acks only after persist** (at-least-once).
- ⬜ orderId generation (server-side, UUID) on create; `event_id` dedupe so replayed creates don't mint duplicates.
- ⬜ (Optional, defense-in-depth) hashed worker pool: hash `order_id` → same worker → per-order serialization + bounded DB concurrency. Framed as throughput, not correctness (§3.4).

### Phase 4 — Server wiring (`main.go`)
- ⬜ Env-based config (no hardcoded DSN / broker URL).
- ⬜ Start HTTP server + broker consumer; **graceful shutdown** (SIGINT → stop consuming, drain in-flight, close pool).

### Phase 5 — Generator (Python)
- ⬜ Continuous mixed-event loop, configurable rate (env/flag), publishes JSON events to the broker.
- ⬜ Discovers real orderIds via `GET /orders` (D6) so updates reference existing orders (§4.1).
- ⬜ Deliberately emit some out-of-order / duplicate events to prove robustness.

### Phase 6 — (OPTIONAL) Dashboard — only if time remains (see D3).

### Phase 7 — Docs & runnability
- ⬜ `README.md`: prerequisites, `docker-compose up`, run service, run generator, test API.
- ⬜ `API.md`: full HTTP endpoint spec + request/response examples + the broker event schema.
- ⬜ `DESIGN.md`: architecture, data model, event flow, and a direct answer to **every** §5 bullet.
- ⬜ `docker-compose.yml`: add broker + ideally the Go service; `up` comes clean.

---

## 5. §5 edge cases → where each is answered (interview cheat-sheet)
| §5 concern | Our answer | Section |
|---|---|---|
| Out-of-order events | nullable fields + LWW guard; update-before-create defensive merge | 3.2, 3.3, 3.7 |
| Concurrency | atomic conditional UPSERT (+ optional hashed pool) | 3.4 |
| Duplicates / replay | updates: LWW no-op; creates: deterministic id + PK conflict | 3.5 |
| Status transitions | enforced state machine, invalid logged not dropped | D4 |
| Throughput | JetStream buffers/backpressure + bounded worker pool + indexed reads | 3.4, D5 |
| "Latest" guarantees | event-time ordering, not arrival time | 3.1, 3.2 |

---

## 6. Definition of done
- [ ] `docker-compose up` brings up all deps cleanly.
- [ ] Service consumes a continuous stream without loss under normal load.
- [ ] List API returns accurate latest state; pagination + status filter + sort-by-updated work.
- [ ] Out-of-order + duplicate events demonstrably handled (show via generator).
- [ ] Graceful shutdown, env config, no debug cruft.
- [ ] README / API.md / DESIGN.md complete; DESIGN answers all §5 bullets.
