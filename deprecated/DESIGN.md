# DESIGN.md

## 1. Architecture

```
┌───────────┐  publish  ┌──────────────┐  consume   ┌───────────────────────────────┐  HTTP/JSON  ┌──────────┐
│ Generator │ ────────▶ │ NATS JetStream│ ─────────▶ │        Go Backend Service      │ ◀────────── │ Reviewer │
│ (Python)  │  events   │ stream ORDERS │  durable   │ decode → derive ID → worker    │   GET /orders│  curl    │
└───────────┘           │ subj: orders  │  consumer  │ pool → conditional UPSERT → ack│             └──────────┘
      ▲                 │   .events     │            └───────────────┬───────────────┘
      │ discovers orderIds via GET /orders (throttled cache)         │
      └───────────────────────────────────────────────────┐          ▼
                                                           │   ┌─────────────┐
                                                           └───│  PostgreSQL │  (current state, 1 row/order)
                                                               └─────────────┘
```

Two processes: the Python **generator** (a producer only — never touches
the database) and the Go **backend service** (consumer + query API). They
communicate exclusively through NATS JetStream; the generator learns real
order IDs only by reading them back from the HTTP API, never from a side
channel — which matters for one of the §5 answers below (out-of-order
events).

### Why NATS JetStream as the transport (not gRPC, not a plain HTTP POST, not Kafka)

The assignment explicitly frames this as an event-driven, runtime-state
problem, so a real durable stream — not a direct call — was the better fit
for demonstrating that:
- **Durability + at-least-once delivery**: JetStream persists messages and
  requires an explicit ack; a consumer crash mid-processing doesn't lose
  the event, it gets redelivered. A direct HTTP call from generator to
  service would need its own retry/backoff/persistence logic to get the
  same guarantee — reinventing what a broker already does.
- **Decoupling**: the generator can run (and keep publishing) even if the
  Go service is temporarily down; messages queue in the stream until a
  consumer is available.
- **A first-class Go client with a clean modern API** (`nats-io/nats.go`'s
  `jetstream` package) made the durable-consumer + explicit-ack model
  straightforward to implement and easy to explain.
- Kafka would give similar guarantees but is heavier operationally for a
  take-home's scope; NATS JetStream is a single lightweight container.

### Why HTTP/JSON for the query API (not gRPC)

`curl`-able, human-readable, trivial to demo pagination/filter/sort with
a browser or `curl` — directly serves the "runnability" evaluation
criterion. gRPC would add proto tooling for no benefit here, since there's
no other service consuming this API programmatically.

---

## 2. Data model

Single table, one row per order — the "current state" table:

```sql
CREATE TABLE orders (
    order_id        TEXT PRIMARY KEY,
    customer_id     TEXT,               -- nullable: see "out-of-order" below
    restaurant_id   TEXT,               -- nullable: same
    status          TEXT,               -- nullable: same
    items           JSONB,              -- nullable: same
    last_event_at   TIMESTAMPTZ NOT NULL, -- EVENT time — the ordering key
    last_updated_at TIMESTAMPTZ         -- WALL-CLOCK time — what the API reports
);
```

Two different "when" columns, deliberately:
- **`last_event_at`** is the `occurredAt` from whichever event most
  recently passed the last-write-wins check. It's the *ordering key* —
  comparisons for "is this event newer" always use this, never wall clock.
- **`last_updated_at`** is set to `now()` every time a write actually
  happens. It's what `GET /orders`'s `lastUpdated` field reports, and what
  `sort=last_updated_desc/asc` sorts by — i.e. "when did our system last
  learn something new about this order," which is what a reviewer watching
  a live dashboard actually wants (recency of *our* knowledge), as opposed
  to the event's own claimed timestamp (which could be stale/backdated on
  purpose, per the generator's chaos injection).

`customer_id`, `restaurant_id`, `status`, and `items` are all nullable —
not because their real-world value can be null, but because our own
knowledge of them can be incomplete for a brief window (see §5.1).

---

## 3. Event flow

1. **Generator** builds a JSON event (`models.OrderEvent`'s wire shape,
   documented in `API.md`) and publishes it to JetStream subject
   `orders.events` (`generator/generate.py`).
2. **`broker.Consumer.Run`** (`broker/consumer.go`) pulls the next message,
   decodes it, and for `order.create` events derives the order's ID
   server-side: `orderId = UUIDv5(ORDER_NAMESPACE, eventId)`. Malformed
   messages (bad JSON, or a non-create event with no `orderId`) are
   `Term()`'d — permanently dropped, since redelivery can never fix a
   parse error; this is boundary validation on data arriving from outside
   the system.
3. The decoded event is handed to **`broker.Pool`** (`broker/pool.go`),
   which hashes `orderId` to pick one of N worker goroutines — all events
   for the same order always land on the same worker, so they're applied
   strictly in submission order, while different orders process fully in
   parallel.
4. The worker calls **`db.Repository.ApplyEvent`** (`db/repository.go`),
   a single conditional `UPSERT` — this is where all the correctness
   guarantees actually live (§5 below). Only on success does the worker
   **ack** the JetStream message; on any error it **nak**s (redelivery).
5. **`server.Server`** (`server/handlers.go`) serves `GET /orders` by
   reading straight from the same table — there is no separate read
   model/cache to go stale.

---

## 4. §5 Design considerations — answered

### 5.1 Out-of-order events

**Two layers.** First, *within* a normal create→update sequence, ordering
is by `occurredAt` (event time), not arrival time — an event that claims
to have happened before whatever we already recorded is a no-op, no
matter when it physically arrived (see 5.3's LWW mechanism). This is what
"latest" actually means here (also answers 5.6).

Second, the specific edge case the brief calls out: **an update arriving
for an order we haven't "created" yet.** This is structurally impossible
via the generator's normal flow — the generator only ever sends an update
for an order ID it read back from `GET /orders`, and that ID only exists
in the API's response *after* the create has already been persisted
(`plan.md` D6/§3.7 has the full timing proof). But "structurally
impossible via our own generator" isn't the same as "impossible" — a
real production system doesn't control every producer, and the brief
asks what we *do* about it, not just whether it can happen. So it's
handled defensively anyway, proven via `generator/demo_hook.py`, which
publishes a raw update straight to JetStream, bypassing discovery
entirely:
- The update's `ApplyEvent` call does a normal `INSERT ... ON CONFLICT`.
  Since no row exists yet, it's a plain `INSERT` — a **partial row**,
  keyed by the update's `orderId`, with only the fields that update
  carries populated; `customer_id`/`restaurant_id` (and possibly `items`
  or `status`, whichever this particular event doesn't carry) stay `NULL`.
- When the real create event eventually arrives (or never does, in the
  demo-hook case), it fills in the `NULL` customer/restaurant fields
  *without* overwriting a status the partial row may already have — the
  same last-write-wins guard (5.3) protects this merge, since a create's
  `occurredAt` is always earlier than any status change that could have
  happened to that order.
- **D4 nuance:** the state-machine transition check (5.4) only applies
  from an order's *second* status-touching event onward. A partial row
  from an update-first insert has no prior "Received" baseline to
  validate a transition against, so the first status this order ever sees
  is accepted outright, whatever it is.

### 5.2 Concurrency

The correctness mechanism is the **atomic conditional `UPSERT`** itself —
a single SQL statement, so Postgres's own row-level locking makes it
correct regardless of how many goroutines (or even separate service
instances) hit the same `order_id` concurrently. There's no
read-then-write race window because there's no separate read step.

The hashed worker pool (`broker/pool.go`) sitting in front of it is a
**throughput optimization, not the correctness guarantee** — routing all
of one order's events to the same goroutine means they're *submitted* to
the database in a sane order (matching however JetStream delivered them),
which reduces contention and wasted lock-wait time, but the UPSERT would
still be correct even with random routing or a single-threaded consumer.
This separation was a deliberate design choice: it means the throughput
layer can be tuned (`WORKER_COUNT`) or even removed without touching
correctness at all.

### 5.3 Duplicates / replayed events

Split by event type, because they need different mechanisms:

- **Updates are idempotent for free.** A replayed update carries the same
  `occurredAt` as the original. The UPSERT's guard is a *strict*
  inequality — `WHERE excluded.last_event_at > orders.last_event_at` —
  so a replay with an identical (not-newer) timestamp always fails the
  guard and becomes a no-op. No dedup table needed.
- **Creates need a stable ID**, because a random ID per delivery would
  defeat the `ON CONFLICT` entirely — a replayed create would just look
  like a brand new order. The fix: `orderId = UUIDv5(ORDER_NAMESPACE,
  eventId)` (`broker/consumer.go`). A replayed create (same `eventId`)
  always derives the *same* `orderId`, so it hits the same primary key
  and becomes a genuine conflict → no-op, not a duplicate row. Verified
  live in Phase 3/4 testing: publishing the exact same create event twice
  produced exactly one row.

### 5.4 Status transitions

Enforced, not left wide open: `Received → Preparing → Complete`;
`Cancelled` reachable from either non-terminal state; `Complete` and
`Cancelled` are terminal (nothing transitions out of them). This is
encoded directly in the UPSERT's `WHERE` clause (`db/repository.go`,
`applyEventSQL`) as part of the *same* atomic condition as the
last-write-wins timestamp check — so an invalid transition can't slip
through a race window either.

Per the brief's instruction to make a decision and justify it: I chose
**enforce + log + count, don't silently drop and don't error the whole
event out**. Rationale — a bogus transition (e.g. `Complete → Preparing`)
almost certainly indicates a bug or a bad actor upstream, and pretending
it succeeded would corrupt state; but killing the whole pipeline on it
would let one bad event take down otherwise-healthy processing. Logging
+ counting (`Repository.InvalidTransitionCount()`) keeps it observable
without being disruptive. Verified live: the generator's intentionally
random status choices produced real invalid-transition rejections in the
service log during Phase 5 testing (12 in one 15-second run).

One extra piece of engineering worth calling out: the UPSERT alone can't
tell *why* it had no effect (a stale timestamp and an invalid transition
both just show up as "0 rows affected"). Conflating them would make the
invalid-transition count meaningless. So on that rare no-op path only
(never on the normal accepted-event path), `ApplyEvent` does one cheap
follow-up read to disambiguate: if the event's timestamp isn't even newer
than what's stored, it's logged as stale/duplicate regardless of what the
transition would have been (since even a *valid* transition at that same
old timestamp would've been rejected too); only if the timestamp **is**
newer and the transition is still invalid does it count against the D4
metric.

### 5.5 Throughput

Several independent layers, each addressing a different bottleneck:
- **JetStream itself** buffers and applies backpressure — the generator
  can publish faster than the service consumes without either side
  blocking or dropping data; messages queue durably in the stream.
- **The hashed worker pool** (§5.2) parallelizes across orders while
  serializing within an order, so throughput scales with `WORKER_COUNT`
  rather than being limited to one event at a time.
- **Bounded DB concurrency**: `pgxpool` caps the number of concurrent
  Postgres connections, so a burst of events can't overwhelm the
  database with unbounded connections — backpressure shows up as workers
  waiting on the pool, not as Postgres falling over.
- **Indexed reads**: `idx_orders_status` and `idx_orders_last_updated`
  keep `GET /orders`'s filter/sort paths off a full table scan as the
  order count grows.
- All of `WORKER_COUNT`, `WORKER_BUFFER_SIZE`, and the HTTP/DB
  configuration are environment-tunable (`main.go`), so this can be
  scaled without a code change.

### 5.6 "Latest" guarantees

"Latest" is defined by **event time** (`occurredAt`), not arrival time —
this is the single idea threading through 5.1, 5.2, and 5.3. The API's
`lastUpdated` field, however, reports **wall-clock write time**
(`last_updated_at`), which is a deliberate and different guarantee: it
answers "when did our system last learn something new," which is what
sorting/monitoring actually wants, while `last_event_at` (not exposed in
the API, but present in the schema) is what the correctness logic
actually orders by internally. Conflating these two would either make
sorting-by-recency meaningless (if a backdated event kept "winning" the
sort) or make correctness fragile (if wall-clock write order decided
which event was newer instead of the event's own claimed time).

---

## 5. Known limitations / explicitly out of scope

- **Dashboard (D3):** cut from scope per the plan, in favor of spending
  the available time on the correctness/event-processing core the brief
  weights highest. `curl`/`GET /orders` is the demo surface instead.
- **`InvalidTransitionCount()` is in-memory**, reset on service restart —
  fine as an observability signal for a demo, not durable metrics. A
  production system would export this to Prometheus/similar instead.
- **Single NATS/Postgres instance**, no replication/HA — appropriate for
  a take-home; the architecture (durable stream + conditional UPSERT)
  doesn't change if either were made highly available later, since
  neither the ack-after-persist protocol nor the LWW UPSERT depend on
  single-instance behavior.
- **JetStream storage is durable but local** (a Docker volume, not
  replicated) — data survives a container restart but not a lost volume;
  acceptable for this scope, called out here rather than glossed over.
