# Design

## 1. Architecture

```
  ┌─────────────┐      publish       ┌──────────────────┐      consume       ┌──────────────────────┐
  │  Generator  │  ───────────────▶  │  NATS JetStream  │  ───────────────▶  │    Go Backend        │
  │  (Python)   │   orders.events    │  stream: ORDERS  │  durable consumer  │  decode → route →    │
  │  producer   │                    │  (persistent)    │                    │  UPSERT → ack        │
  └─────────────┘                    └──────────────────┘                    └──────────┬───────────┘
         ▲                                                                               │
         │  discovers orderIds                                                           │  write
         │  via GET /orders                                                              ▼
         │                                                                    ┌──────────────────────┐
         │                                                                    │     PostgreSQL       │
         │                                                                    │  current state       │
         │                                                                    │  (1 row per order)   │
         │                                                                    └──────────┬───────────┘
         │                                                                               │  read
         │                                                                    ┌──────────▼───────────┐
         └────────────────────────────────────────────────────────────────  │  HTTP API  GET /orders│  ◀── Reviewer (curl)
                                                                              └──────────────────────┘
```

Two processes communicating only through NATS JetStream:

- **Generator** (Python) — a producer only; never touches the database. It learns real order IDs solely by reading them back from `GET /orders`, never via a side channel.
- **Backend** (Go) — consumer + query API. It persists state and serves reads from the same table.

**Why JetStream (not gRPC / HTTP POST / Kafka):** the brief frames this as an event-driven, durable-state problem. JetStream gives at-least-once delivery with explicit ack (a crash mid-processing redelivers, not drops) and decoupling (generator keeps publishing while the service is down). Kafka offers the same but is heavier; JetStream is one lightweight container.

**Why HTTP/JSON for the query API (not gRPC):** it's `curl`-able and human-readable, which directly serves the runnability criterion. No other service consumes it programmatically, so proto tooling would add cost for no gain.

---

## 2. Data model

One table, one row per order — the current-state table:

```sql
CREATE TABLE orders (
    order_id        TEXT PRIMARY KEY,
    customer_id     TEXT,                  -- nullable: knowledge may be incomplete
    restaurant_id   TEXT,                  -- nullable
    status          TEXT,                  -- nullable
    items           JSONB,                 -- nullable
    last_event_at   TIMESTAMPTZ NOT NULL,  -- EVENT time — the ordering key
    last_updated_at TIMESTAMPTZ            -- WALL-CLOCK time — what the API reports
);
```

Two deliberate "when" columns:

- **`last_event_at`** — the `occurredAt` of the winning event. This is the *ordering key*: "is this event newer" always compares this, never wall clock.
- **`last_updated_at`** — `now()` on every actual write. This is what `GET /orders` reports as `lastUpdated` and what `sort=last_updated_*` uses — i.e. "when our system last learned something," which is what a live dashboard wants.

Fields are nullable because *our knowledge* of them can be briefly incomplete (see §5.1), not because the real values can be null.

---

## 3. Event flow

1. **Generator** publishes a JSON event to subject `orders.events`.
2. **Consumer** (`broker/consumer.go`) pulls a message, decodes it, and for `order.create` derives the ID server-side: `orderId = UUIDv5(ORDER_NAMESPACE, eventId)`. Malformed messages are `Term()`'d (dropped) — redelivery can't fix a parse error.
3. **Worker pool** (`broker/pool.go`) hashes `orderId` to a worker, so all events for one order apply in submission order while different orders run in parallel.
4. **`Repository.ApplyEvent`** (`db/repository.go`) runs one conditional `UPSERT` — where all correctness lives. Ack on success, nak (redeliver) on error.
5. **`GET /orders`** (`server/handlers.go`) reads from the same table — no separate read model to go stale.

---

## 4. Design considerations

### 4.1 Out-of-order events

Two layers. **Within** a create→update sequence, ordering is by `occurredAt`, not arrival — an event claiming to predate what we recorded is a no-op (see §4.3 LWW). **The called-out edge case** — an update arriving before its create — can't happen via the generator (it only updates IDs read back from `GET /orders`, which exist only after the create persisted), but is handled defensively anyway (proven by `generator/demo_hook.py`):

- The update's UPSERT becomes a plain `INSERT` of a **partial row** — only that event's fields populated; the rest stay `NULL`.
- A later create fills the `NULL` customer/restaurant fields without clobbering an existing status, guarded by the same LWW check.
- The transition check (§4.4) applies only from an order's *second* status event on, so a partial row's first status is accepted outright.

### 4.2 Concurrency

The correctness mechanism is the **atomic conditional UPSERT** — a single SQL statement, so Postgres row-level locking makes it correct regardless of goroutine or instance count. No read-then-write race, because there's no separate read.

The hashed worker pool is a **throughput optimization, not the guarantee** — it reduces lock contention but the UPSERT stays correct even with random routing or a single consumer. So `WORKER_COUNT` can be tuned or removed without touching correctness.

### 4.3 Duplicates / replays

- **Updates are idempotent for free:** the guard is *strict* — `WHERE excluded.last_event_at > orders.last_event_at` — so a replay with the same timestamp is a no-op. No dedup table.
- **Creates need a stable ID:** `orderId = UUIDv5(ORDER_NAMESPACE, eventId)`. A replayed create derives the same ID, hits the same primary key, and becomes a conflict → no-op, not a duplicate row.

### 4.4 Status transitions

Enforced: `Received → Preparing → Complete`; `Cancelled` from either non-terminal state; `Complete`/`Cancelled` terminal. Encoded in the same UPSERT `WHERE` clause as the LWW check, so invalid transitions can't slip through a race either.

Decision: **enforce + log + count** (`Repository.InvalidTransitionCount()`), don't silently drop or error the pipeline. A bogus transition signals an upstream bug; accepting it corrupts state, but killing the pipeline lets one bad event take down healthy processing.

Nuance: a no-op UPSERT can't say *why* (stale timestamp vs. invalid transition both show "0 rows"). Only on that rare no-op path, `ApplyEvent` does one cheap read to disambiguate — stale/duplicate is logged, and only a *newer* timestamp with an invalid transition counts against the metric.

### 4.5 Throughput

- **JetStream** buffers and applies backpressure — publish faster than consume without loss.
- **Hashed worker pool** parallelizes across orders; scales with `WORKER_COUNT`.
- **`pgxpool`** caps concurrent DB connections — a burst backs up on workers, not on Postgres.
- **Indexes** (`idx_orders_status`, `idx_orders_last_updated`) keep filter/sort off full scans.
- `WORKER_COUNT`, `WORKER_BUFFER_SIZE`, and HTTP/DB config are all env-tunable.

### 4.6 "Latest" guarantee

"Latest" = **event time** (`occurredAt`), the idea threading §4.1–4.3. The API's `lastUpdated` reports **wall-clock write time** — a deliberately different guarantee answering "when did we last learn something," which is what sorting/monitoring wants. Conflating them would either break recency sorting (a backdated event keeps winning) or make correctness fragile (wall-clock order deciding which event is newer).
