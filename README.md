# Vaurd Backend Assignment — Food Delivery Order Processing Service

A small event-driven backend that ingests a continuous stream of food-order
events (creates, status updates, item updates), keeps each order's current
state correct under duplicates/out-of-order delivery, and serves that state
over a simple HTTP API.

- **Go backend service** — consumes events from NATS JetStream, persists
  current order state to Postgres, serves `GET /orders`.
- **Python generator** — publishes a continuous, randomized stream of order
  events (and deliberately injects duplicates/out-of-order events).

See `DESIGN.md` for the architecture and reasoning, `API.md` for the full
HTTP contract. This file is just: how do I actually run it.

---

## Prerequisites

- **Docker Desktop** (for Postgres + NATS) — any recent version.
- **Go 1.25+** — to run the backend service.
- **Python 3.10+** and `pip` — to run the generator.

Everything below assumes you're in the repo root.

---

## Step 1 — Start the dependencies (Postgres + NATS)

```bash
docker compose up -d
```
This starts two containers in the background (`-d`):
- `postgres_db` — Postgres 15, exposed on `localhost:5432`, credentials/DB
  name baked into `docker-compose.yml` (`vaurd_user` / `vaurd_password` /
  `food_delivery`).
- `nats` — NATS server with JetStream enabled (`-js`), exposed on
  `localhost:4222` (client connections) and `localhost:8222` (monitoring/
  health endpoint used by Docker's own healthcheck).

Confirm both are healthy before moving on:
```bash
docker compose ps
```
Both `food_delivery_db` and `food_delivery_nats` should show `Up ... (healthy)`.

---

## Step 2 — Run the Go backend service

```bash
go run main.go
```
This does, in order: connects to Postgres and runs migrations (creates the
`orders` table if it doesn't exist), connects to NATS and ensures the
durable `ORDERS` stream + `orders-service` consumer exist, then starts
consuming events in the background while serving HTTP on `:8080`.

You should see log lines ending with something like:
```
HTTP server listening on :8080
```

All configuration is via environment variables, each with a default that
matches the `docker-compose.yml` setup above — so the plain command works
out of the box. Override any of them if needed:

| Variable             | Default                                                                          | Meaning                                    |
|----------------------|-----------------------------------------------------------------------------------|---------------------------------------------|
| `DATABASE_URL`       | `postgres://vaurd_user:vaurd_password@localhost:5432/food_delivery?sslmode=disable` | Postgres connection string                  |
| `NATS_URL`           | `nats://localhost:4222`                                                          | NATS server address                         |
| `HTTP_PORT`          | `8080`                                                                            | Port the List Orders API listens on         |
| `WORKER_COUNT`       | `8`                                                                               | Number of concurrent event-processing workers |
| `WORKER_BUFFER_SIZE` | `64`                                                                              | Per-worker channel buffer size              |

Leave this running in its own terminal — it's a long-running service.
Press **Ctrl+C** at any point to shut it down gracefully (it stops
consuming new events, finishes any in-flight ones, then exits).

---

## Step 3 — Install and run the generator

In a **new terminal** (leave the Go service running):

```bash
pip install -r requirements.txt
```
Installs the two Python dependencies: `nats-py` (JetStream client) and
`requests` (used to poll `GET /orders`).

```bash
python generator/generate.py
```
This connects to NATS and starts publishing a continuous, randomized mix
of `order.create` / `order.update.status` / `order.update.items` events —
forever, until you stop it with **Ctrl+C**. It also:
- periodically calls `GET /orders` to learn real order IDs before sending
  updates for them (never invents an update for an order it hasn't seen),
- occasionally republishes the exact same event twice (tests duplicate
  handling),
- occasionally backdates a status update's timestamp (tests out-of-order
  handling).

Rate and behavior are configurable via env vars (all optional):

| Variable                     | Default | Meaning                                            |
|-------------------------------|---------|-----------------------------------------------------|
| `NATS_URL`                    | `nats://localhost:4222` | NATS server address                    |
| `API_BASE_URL`                 | `http://localhost:8080` | Base URL of the Go service's HTTP API |
| `EVENT_INTERVAL_SECONDS`       | `0.5`   | Delay between published events (lower = faster)    |
| `DISCOVERY_INTERVAL_SECONDS`   | `5`     | How often to refresh the known-order-IDs cache      |
| `DUPLICATE_PROBABILITY`        | `0.1`   | Chance of replaying the previous event verbatim     |
| `STALE_UPDATE_PROBABILITY`     | `0.15`  | Chance a status update is deliberately backdated    |

Example — run it faster, for a livelier demo:
```bash
EVENT_INTERVAL_SECONDS=0.1 python generator/generate.py
```
(On Windows PowerShell: `$env:EVENT_INTERVAL_SECONDS=0.1; python generator/generate.py`)

Let it run for at least the discovery interval (5s default) so it has a
chance to learn some order IDs and start sending updates, not just creates.

---

## Step 4 — Query the API

In a third terminal, while the service and generator are both running:

```bash
curl http://localhost:8080/healthz
```
Returns `200 OK` if the service is up and the database is reachable.

```bash
curl "http://localhost:8080/orders?limit=5"
```
Returns the 5 most recently updated orders, newest first (the default sort).

```bash
curl "http://localhost:8080/orders?status=Preparing&sort=last_updated_asc&limit=10&offset=0"
```
Filters to orders currently `Preparing`, oldest-updated first, page size 10.
See `API.md` for the full parameter/response reference.

---

## Step 5 (optional) — Prove the "update before create" edge case

This is a one-shot script, separate from the continuous generator, that
publishes a single raw status-update event for an order ID that was
**never created** — bypassing the generator's normal discovery flow
entirely:

```bash
python generator/demo_hook.py
```
It prints the order ID it used. Then check:
```bash
curl "http://localhost:8080/orders?status=Preparing"
```
and find that order ID in the results — its `customerId`/`restaurantId`
fields will be absent (unknown), since no create event for it has arrived.
This demonstrates the service builds a partial record instead of dropping
the "orphan" update. See `DESIGN.md` for why this is safe.

---

## Shutting everything down

- Stop the generator: **Ctrl+C** in its terminal.
- Stop the Go service: **Ctrl+C** in its terminal (graceful shutdown).
- Stop the dependencies:
  ```bash
  docker compose down
  ```
  Add `-v` (`docker compose down -v`) if you also want to wipe the
  Postgres/NATS data volumes and start completely fresh next time.

---

## Repository layout

| Path            | Contents                                                       |
|-----------------|-----------------------------------------------------------------|
| `models/`       | Shared domain types (`Order`, `OrderEvent`, status/event enums) |
| `api/`          | HTTP request/response DTOs (wire contract only, no logic)      |
| `db/`           | Postgres access: migrations, `Repository` (event apply + list)  |
| `broker/`       | NATS JetStream consumer + hashed worker pool                    |
| `server/`       | HTTP handlers (`GET /orders`, `GET /healthz`)                   |
| `main.go`       | Wiring, env config, graceful shutdown                           |
| `generator/`    | Python event generator + the update-before-create demo hook     |
| `docker-compose.yml` | Postgres + NATS (JetStream) dependencies                   |
