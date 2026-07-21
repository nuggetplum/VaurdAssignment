# DESIGN.md

Go service reads order events off NATS JetStream, keeps one row per order in
Postgres, serves it at `GET /orders`. Python generator publishes the events,
including deliberate duplicates and backdated ones so the edge cases are visible
while it runs.

```
┌───────────┐  publish   ┌─────────────────┐  consume    ┌────────────────────────────┐  HTTP/JSON  ┌──────────┐
│ Generator │ ──────────▶│  NATS JetStream  │ ───────────▶│      Go Backend Service     │ ◀────────── │ Reviewer │
│ (Python)  │   events   │  stream ORDERS   │  durable    │  decode → derive ID →       │   GET      │  curl    │
└───────────┘            │  subj:           │  consumer   │  worker pool → UPSERT → ack │  /orders   └──────────┘
      ▲                   │  orders.events  │             └──────────────┬─────────────┘
      │                   └─────────────────┘                            │
      │                            │                                     ▼
      │                            │                              ┌─────────────┐
      │                            └──────────────────────────────│  PostgreSQL  │
      │ discovers orderIds via                                    │ (1 row/order)│
      │ GET /orders (throttled cache)                              └─────────────┘
      └───────────────────────────────────────────────────────────────────────────
```

**The one idea that matters:** every correctness guarantee lives in a single
conditional UPSERT in `db/repository.go`. Last-write-wins and the status state
machine are one atomic `WHERE` clause. Nothing else in the system needs to be
correct for state to stay correct.

## Edge cases and solutions

| Concern | Decision | Where |
|---|---|---|
| Out-of-order | Order by event time; insert a partial row if the create hasn't landed | §5.1 |
| Concurrency | Atomic UPSERT is the guarantee; worker pool is only throughput | §5.2 |
| Duplicates | Updates no-op via strict `>`; creates get a deterministic UUIDv5 ID | §5.3 |
| Status transitions | Enforced in SQL. Log + count, don't drop, don't nak | §5.4 |
| Throughput | JetStream buffers, pool parallelizes per order, blocking = backpressure | §5.5 |
| "Latest" | Event time for correctness, wall clock for sorting — on purpose | §5.6 |

## How it works

**Transport: JetStream, not a direct call.** Durable, explicit ack, so a crash
mid-event redelivers orders instead of losing it. Generator keeps publishing if the
service is down. Considered using kafka as it gives the same guarantees but is much heavier to run for this scope.

**API: HTTP/JSON.** Simple HTTP for this demo. In a production enviroment where it is consumed by other programs and bears more load, gRPC would be the better option. 

```sql
CREATE TABLE orders (
    order_id        TEXT PRIMARY KEY,
    customer_id     TEXT,
    restaurant_id   TEXT,
    status          TEXT,
    items           JSONB,
    last_event_at   TIMESTAMPTZ NOT NULL,   -- event time, the ordering key
    last_updated_at TIMESTAMPTZ            -- wall clock, what the API returns
);
```

Two timestamps because they answer different questions — see §5.6. Data columns
are nullable because our knowledge can be incomplete, not because the real
values can be null (§5.1).

Flow:

1. Publish JSON to `orders.events`.
2. Consumer decodes. Bad JSON / no `eventId` / update with no `orderId` →
   `Term()`. Dropped for good, since redelivery can't fix a parse error.
3. On `order.create` only, derive `orderId = UUIDv5(NAMESPACE, eventId)`.
4. Pool hashes `orderId` → same order always hits the same goroutine.
5. Worker runs the UPSERT. Ack on success, nak on error.
6. `GET /orders` reads the same table. No separate read model to go stale.

## §5.1 Out-of-order events

Ordering is by `occurredAt`, never arrival time.
`WHERE excluded.last_event_at > orders.last_event_at` is included to guard against out of order events, so an older event is a
no-op whenever it shows up.

The case the brief asks about — **an update for an order we never created:**

- Can't happen via our generator (it only updates IDs it read back from the
  API), but we don't control every producer. Handled anyway, and
  `generator/demo_hook.py` proves it by publishing a raw update straight to
  JetStream.
- No row exists → plain INSERT → **partial row**, unknown fields NULL. We keep
  the event rather than dropping an orphan.
- **The tradeoff:** a late create does *not* backfill customer/restaurant. It
  carries `status: Received`, and since LWW and the state machine are one
  combined condition, a row already at `Preparing` rejects the whole update —
  names included. State machine beats backfilling. Deliberate: the alternative
  is splitting the condition, which reopens a race. A real fix is a background
  reconciler, out of scope here. Walked through live in `test.md` scenario 5.
- The state machine only applies from an order's *second* status event. A
  partial row has no baseline, so its first status is accepted whatever it is.

## §5.2 Concurrency

- Correctness = the atomic conditional UPSERT. One statement, so Postgres row
  locking handles any number of goroutines *or* service instances. No
  read-then-write gap because there's no separate read.
- The hashed worker pool is a **throughput optimization, not the guarantee.**
  Routing one order's events to one goroutine just cuts lock contention. The
  UPSERT stays correct with random routing or a single-threaded consumer.
- That separation is the point: `WORKER_COUNT` is tunable, and the pool could be
  deleted entirely, without touching correctness.
- These are real goroutines scheduled across cores, not single-threaded async.

## §5.3 Duplicate / replayed events

Two mechanisms, because creates and updates need different things:

- **Updates — idempotent for free.** A replay carries the same `occurredAt`, and
  the guard is a strict `>`, so it fails. No dedup table needed.
- **Creates — need a stable ID.** A random ID per delivery would make a replay
  look like a new order and `ON CONFLICT` would never fire. So
  `orderId = UUIDv5(NAMESPACE, eventId)` — same event, same PK, real conflict →
  `DO UPDATE` → identical timestamp then fails LWW. Both steps are load-bearing;
  the PK conflict alone wouldn't stop it.

## §5.4 Status transitions

Enforced: `Received -> Preparing -> Complete`, `Cancelled` from either
non-terminal state, `Complete`/`Cancelled` terminal. Written into the same
`WHERE` clause as the timestamp check, so it's atomic and can't race.

- **Decision: enforce, log, count.** Silently dropping corrupts state. Naking
  lets one bad event jam the pipeline forever. Logging keeps it visible without
  being disruptive.
- "0 rows affected" looks identical for a stale timestamp and a bad transition,
  so on that path only, one follow-up read tells them apart. Timestamp is
  checked first — if the event isn't newer, that alone explains it.
- Caveat: that read was meant to be a rare path and isn't. Orders drift into
  terminal states, and from terminal most of the generator's random status picks
  get rejected, so a fair chunk of status updates take the extra read.

## §5.5 Throughput

- JetStream buffers — the generator can outrun the service, nothing drops.
- The pool parallelizes across orders while serializing within one.
- `WORKER_COUNT` / `WORKER_BUFFER_SIZE` are env-tunable. `Submit` blocks on a
  full buffer, and that block is the backpressure signal.
- **Honest gap — the DB pool isn't tuned.** `InitDB` never sets `MaxConns`, so
  pgx defaults to `max(4, NumCPU)`. At `WORKER_COUNT=8` on 4 cores, half the
  workers just wait on connections. Effective parallelism is the smaller number
  and the two knobs don't know about each other.
- **Honest gap — indexes help less than they look.**
  `WHERE ($1 = '' OR status = $1)` isn't sargable, so `idx_orders_status` goes
  unused, and there's no composite `(status, last_updated_at)` for the
  filter-and-sort path. The plain sort is covered.

## §5.6 "Latest" guarantees

- Correctness orders by **event time** (`occurredAt`). That's §5.1–5.3 in one line.
- The API's `lastUpdated` reports **wall-clock write time** instead, on purpose.
- Mixing them breaks something either way: sort by event time and a backdated
  event pollutes recency ordering; decide correctness by write time and arrival
  order silently overrides what the event actually claims.
- So `last_event_at` is internal and `last_updated_at` is what you see.

## Known limitations

- **No automated testing.** Everything above rests on one uncovered SQL statement.
- **Single NATS/Postgres, no HA**, and JetStream storage is a local Docker
  volume. Fine for this scope. Nothing in the design depends on single-instance
  behaviour, so going HA wouldn't change the architecture.

Streamlit dashboard in `dashboard/` is optional and not part of the core
deliverable — curl works fine.
