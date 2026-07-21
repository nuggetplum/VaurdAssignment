# Vaurd Backend Assignment - AI Agent Execution Plan

## 🎯 Context & Goal
You are acting as an expert Golang backend engineer. Your goal is to build an event-driven food delivery order processing service. 
The system ingests a continuous stream of order events (create, update status, update items) which may arrive out-of-order, processes them safely in a highly concurrent environment, and exposes a gRPC API returning the latest state of all orders.

## 🏗️ Architecture & Stack
*   **Backend:** Go (Golang)
*   **Database:** PostgreSQL (with `golang-migrate` and `go:embed` for schema management)
*   **Transport / API:** gRPC (Protocol Buffers)
*   **Mock Generator:** Python (gRPC Client)
*   **Dashboard:** Python + Streamlit (gRPC Client)
*   **Module Name:** `github.com/nuggetplum/VaurdAssignment`

## 🚦 Current State
*   Go module initialized.
*   PostgreSQL running via Docker on `localhost:5432` (User: `vaurd_user`, Pass: `vaurd_password`, DB: `food_delivery`).
*   `golang-migrate` is implemented in `db/database.go` using `go:embed`.
*   The database schema (the `orders` table) has been successfully created.

---

## 🗺️ Step-by-Step Execution Roadmap

### Phase 1: Data Contracts (gRPC & Protobuf)
**Objective:** Define the strict boundaries for event ingestion and API consumption.
1.  Create `proto/order.proto`.
2.  Define `Event` (handling out-of-order fields using `optional`), `OrderItem`, and `Order` messages.
3.  Define `OrderService` with two RPCs: `EmitEvent` and `ListOrders`.
4.  Run `protoc` to generate Go stubs into the `pb/` directory.

### Phase 2: Database Repository Layer
**Objective:** Build safe, concurrent database queries using `pgxpool`.
1.  Create `db/repository.go`.
2.  Implement `UpsertOrder(ctx, event)`: 
    *   Must use PostgreSQL `INSERT ... ON CONFLICT (order_id) DO UPDATE`.
    *   Must handle merging out-of-order data (e.g., if a status update arrives before the creation event, insert the status but leave `customer_id` null until the creation event arrives).
    *   Must handle `JSONB` for order items.
3.  Implement `FetchAllOrders(ctx)`: Returns all orders sorted by `last_updated_at DESC`.

### Phase 3: The Concurrency Engine (Coding Challenge Core)
**Objective:** Prevent race conditions when multiple events for the same `order_id` arrive simultaneously.
1.  Create `queue/worker_pool.go`.
2.  Implement a hashed worker pool (e.g., 5-10 Go routines).
3.  Hash the incoming `order_id` to route all events for a specific order to the *same* worker channel.
4.  Workers consume from their channel sequentially and execute the `UpsertOrder` database function.

### Phase 4: gRPC Server Implementation
**Objective:** Connect the gRPC endpoints to the concurrency engine and database.
1.  Create `handlers/grpc_handler.go`.
2.  Implement `EmitEvent`: Push the incoming event payload into the `worker_pool`. Return success immediately (async processing).
3.  Implement `ListOrders`: Query `FetchAllOrders` and map the SQL results to the Protobuf `ListOrdersResponse`.
4.  Update `main.go` to start the gRPC TCP listener on port `50051`.

### Phase 5: Python Order Generator (Script)
**Objective:** Simulate the continuous, chaotic event stream.
1.  Create `generator/main.py`.
2.  Compile the `order.proto` file for Python (`grpcio-tools`).
3.  Write a loop that continuously connects to the Go gRPC server and emits randomized `order.create`, `order.update.status`, and `order.update.items` events.
4.  Ensure some events are intentionally sent out-of-order to test the system's robustness.

### Phase 6: Python Dashboard
**Objective:** Create a visual verifier for the reviewer.
1.  Create `dashboard/app.py` using Streamlit.
2.  Implement a gRPC client that calls `ListOrders` every 2 seconds.
3.  Display the data in a table with a dropdown filter for "Status".

### Phase 7: Polish & Documentation
**Objective:** Meet the strict evaluation criteria.
1.  Generate `API.md`: Document the gRPC service and message structures.
2.  Generate `DESIGN.md`: Document the `UPSERT` database strategy, the hashed worker pool concurrency model, and how idempotency is handled.
3.  Update `README.md` with strict instructions on how to run `docker-compose up`, start the Go server, run the Python generator, and view the Streamlit dashboard.