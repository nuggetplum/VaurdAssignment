# Vaurd Backend Assignment — Food Delivery Order Processing Service

Event-driven backend that ingests a continuous stream of food-order events,
keeps each order's state correct under duplicates and out-of-order delivery,
and serves that state over HTTP.

- **Go service** — consumes NATS JetStream, persists to Postgres, serves `GET /orders`.
- **Python generator** — publishes randomized events, deliberately including
  duplicates and backdated ones.
- **Streamlit dashboard** (optional) — browse orders without curling by hand.

`DESIGN.md` has the architecture and the §5 edge-case answers. `API.md` has the
full HTTP contract. This file is just how to run it.

---

## Quick start

From an **Administrator Git Bash** window in the repo root:

```bash
./run.sh
```

That's the whole thing. It checks prerequisites, offers to install anything
missing, starts Docker Desktop if it isn't running, brings up Postgres + NATS,
waits for both healthchecks, builds and starts the Go service, starts the
generator and dashboard, opens the dashboard in your browser, and then streams
the service log.

**Ctrl+C stops everything it started.** Containers are stopped but data volumes
are kept, so a re-run picks up where you left off.

Roughly 60–90 seconds on a cold start, most of it Docker Desktop booting.

### Before you run it

- **Use Git Bash, not PowerShell or CMD.** It ships with Git for Windows.
- **Run it as Administrator** — right-click Git Bash → *Run as administrator*.
  The script needs this to start the Docker Desktop engine, and it will refuse
  to continue without it rather than failing halfway through.
- **Make the script executable** if git didn't preserve the bit:
  ```bash
  chmod +x run.sh
  ```
- **Ports 8080 and 8501 must be free.** The script checks and tells you what to
  kill if they aren't.
- **First run installs Python packages** into whichever Python is on your PATH.
  If you'd rather keep that isolated, create a venv first and the script will
  use it:
  ```bash
  python -m venv .venv && source .venv/Scripts/activate
  ```

### Flags

| Flag | Effect |
|---|---|
| `--no-dashboard` | Skip Streamlit |
| `--no-generator` | Infra + service only, no event traffic |
| `--skip-checks` | Skip prerequisite verification (faster re-runs) |
| `--down` | Tear down containers **and volumes**, then exit |
| `--help` | Usage |

### If something goes wrong

Logs from the last run are in `.run-logs/` — `service.log`, `generator.log`,
`dashboard.log`. The script prints the relevant log inline when a component
fails to start.

Common cases:

- **"failed to connect to the docker API ... dockerDesktopLinuxEngine"** —
  Docker Desktop isn't running. The script normally starts it for you; if it
  can't, open Docker Desktop manually, wait for the whale icon to go steady,
  and re-run.
- **Docker Desktop hangs on startup** — quit it fully from the tray, run
  `wsl --shutdown` in an elevated PowerShell, relaunch it, then re-run.
- **Port already in use** — `netstat -ano | grep :8080`, then stop that process.
- **Go or Python "not found" right after installing** — close Git Bash and
  reopen as Administrator so PATH refreshes.

---

## Running it manually

If you'd rather drive each piece yourself, or you're not on Windows:

**Prerequisites:** Docker Desktop, Go 1.22+ (1.25 recommended), Python 3.10+.

```bash
# 1. dependencies
docker compose up -d
docker compose ps          # both should read "Up (healthy)"

# 2. backend (leave running)
go run main.go

# 3. generator, in a second terminal
pip install -r requirements.txt
python generator/generate.py

# 4. dashboard, in a third terminal (optional)
pip install -r dashboard/requirements.txt
streamlit run dashboard/app.py
```

Shut down with Ctrl+C in each terminal, then `docker compose down`
(add `-v` to wipe the data volumes).

---

## Configuration

All env vars, all with defaults matching `docker-compose.yml`, so the plain
commands work as-is.

**Service:**

| Variable | Default | Meaning |
|---|---|---|
| `DATABASE_URL` | `postgres://vaurd_user:vaurd_password@localhost:5432/food_delivery?sslmode=disable` | Postgres connection string |
| `NATS_URL` | `nats://localhost:4222` | NATS server address |
| `HTTP_PORT` | `8080` | API port |
| `WORKER_COUNT` | `8` | Concurrent event-processing workers |
| `WORKER_BUFFER_SIZE` | `64` | Per-worker channel buffer |

**Generator:**

| Variable | Default | Meaning |
|---|---|---|
| `NATS_URL` | `nats://localhost:4222` | NATS server address |
| `API_BASE_URL` | `http://localhost:8080` | Where to discover order IDs |
| `EVENT_INTERVAL_SECONDS` | `0.5` | Delay between events (lower = faster) |
| `DISCOVERY_INTERVAL_SECONDS` | `5` | How often to refresh known order IDs |
| `DUPLICATE_PROBABILITY` | `0.1` | Chance of replaying the previous event |
| `STALE_UPDATE_PROBABILITY` | `0.15` | Chance a status update is backdated |

Faster demo: `EVENT_INTERVAL_SECONDS=0.1 ./run.sh`

---

## What to look at

**The API:**

```bash
curl "http://localhost:8080/healthz"
curl "http://localhost:8080/orders?limit=5"
curl "http://localhost:8080/orders?status=Preparing&sort=last_updated_asc&limit=10"
```

**The service log** (streaming in the `run.sh` window) — the edge cases are
visible live:

- `applied event:` — normal processing
- `stale/duplicate status update ignored:` — the last-write-wins guard rejecting
  a backdated event
- `invalid status transition rejected:` — the state machine refusing e.g.
  `Complete → Preparing`

**Testing** — the system uses `generator/manual_simulation.py` for
deterministic, observable integration testing. Unlike the randomized
`generator/generate.py` (which stresses the system with live traffic and
injected chaos), the manual simulation publishes a fixed, pre-planned
sequence of events with pauses between each step so state changes can be
watched live via the API or dashboard.

The six scenarios it runs:

| # | Scenario | What It Verifies |
|---|----------|------------------|
| 1 | Happy path | `Received → Preparing → Complete` lifecycle |
| 2 | Cancellation | `Received → Cancelled` is a valid transition |
| 3 | Items update | Items are replaced (not merged) in JSONB |
| 4 | Invalid transition | `Complete → Preparing` rejected by state machine |
| 5 | Update-before-create | Partial row created on the fly, late create no-op'd |
| 6 | Duplicate + stale | Idempotent replay + LWW rejects backdated events |

Each step prints the published event, queries the API, and displays the
resulting order state. The service log simultaneously shows the server-side
decisions (`applied event:`, `stale/duplicate status update ignored:`,
`invalid status transition rejected:`).

See `planning/test.md` for the full scenario matrix, expected states, and
verification commands.

```bash
# Reset DB (fixed event IDs → truncate between runs)
docker exec -it food_delivery_db psql -U vaurd_user -d food_delivery -c "TRUNCATE TABLE orders;"

# Run the simulation
python generator/manual_simulation.py
```

**The update-before-create case** on its own:

```bash
python generator/demo_hook.py
curl "http://localhost:8080/orders?status=Preparing"
```

Find the printed order ID — `customerId`/`restaurantId` are absent, because no
create event ever arrived for it. See `DESIGN.md` §5.1.

---

## Layout

| Path | Contents |
|---|---|
| `main.go` | Wiring, env config, graceful shutdown |
| `models/` | Domain types (`Order`, `OrderEvent`, enums) |
| `api/` | HTTP request/response DTOs |
| `db/` | Migrations + `Repository` (event apply, list) |
| `broker/` | JetStream consumer + hashed worker pool |
| `server/` | HTTP handlers |
| `generator/` | Event generator, scripted simulation, demo hook |
| `dashboard/` | Optional Streamlit UI |
| `run.sh` | One-command setup and demo |
| `docker-compose.yml` | Postgres + NATS |
