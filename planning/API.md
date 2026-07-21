# API Reference

Base URL (default): `http://localhost:8080`

Two HTTP endpoints (`GET /orders`, `GET /healthz`), plus the JSON envelope
consumed from NATS JetStream, documented here for completeness since it's
the other half of the system's contract.

---

## `GET /orders`

Returns the current state of orders, filtered/sorted/paginated.

### Query parameters (all optional)

| Param    | Type   | Default             | Notes |
|----------|--------|----------------------|-------|
| `status` | string | *(none — no filter)* | One of `Received`, `Preparing`, `Complete`, `Cancelled`. `400` if any other value. |
| `sort`   | string | `last_updated_desc`  | `last_updated_desc` or `last_updated_asc`. `400` if any other value. |
| `limit`  | int    | `50`                 | Clamped to a maximum of `200` (values above 200 are silently capped, not rejected). `400` if negative or not an integer. |
| `offset` | int    | `0`                  | `400` if negative or not an integer. |

### Response — `200 OK`

```json
{
  "orders": [
    {
      "orderId": "60cfd6eb-898c-520d-9fe5-e90bc9699680",
      "customerId": "customer-7",
      "restaurantId": "restaurant-3",
      "status": "Preparing",
      "items": [
        { "itemId": "burger", "qty": 2 },
        { "itemId": "fries", "qty": 1 }
      ],
      "lastUpdated": "2026-07-19T17:52:54.316285+05:30"
    }
  ],
  "total": 53,
  "limit": 50,
  "offset": 0
}
```

| Field                  | Meaning |
|------------------------|---------|
| `orders[].orderId`     | Deterministic UUID (see below on how it's derived from a create event). |
| `orders[].customerId`  | Absent/omitted if unknown (only happens via the update-before-create edge case, see DESIGN.md). |
| `orders[].restaurantId`| Same as `customerId`. |
| `orders[].status`      | One of the four enum values, or `""` if no status-bearing event has been seen yet for this order (same rare edge case). |
| `orders[].items`       | Current item list; `[]` if not yet known. |
| `orders[].lastUpdated` | Wall-clock time this row was last written (RFC3339) — **not** the event's own timestamp. See DESIGN.md for why. |
| `total`                | Total rows matching the filter (ignores `limit`/`offset`), for pagination. |
| `limit`, `offset`      | Echoes back the effective values actually used (after clamping/defaulting). |

### Error responses

| Status | When |
|--------|------|
| `400 Bad Request` | Invalid `status`, `sort`, `limit`, or `offset` value. Body is a plain-text error message. |
| `500 Internal Server Error` | Database error while listing orders. |

### Examples

```bash
curl "http://localhost:8080/orders"
curl "http://localhost:8080/orders?status=Complete"
curl "http://localhost:8080/orders?sort=last_updated_asc&limit=10&offset=20"
```

---

## `GET /healthz`

Liveness/readiness check.

| Status | Meaning |
|--------|---------|
| `200 OK` | Service is up and the database is reachable. |
| `503 Service Unavailable` | Database ping failed. |

No body on success.

```bash
curl -i http://localhost:8080/healthz
```

---

## Broker event envelope (internal contract)

Not an HTTP endpoint, but this is the other half of the system's API
surface: the JSON events published to NATS JetStream (stream `ORDERS`,
subject `orders.events`) that the generator produces and the Go service
consumes.

```json
{
  "eventId":     "3f1e9c2a-...",
  "eventType":   "order.create",
  "occurredAt":  "2026-07-19T17:52:50.069000+00:00",
  "orderId":     "60cfd6eb-...",
  "customerId":  "customer-7",
  "restaurantId":"restaurant-3",
  "status":      "Preparing",
  "items":       [ { "itemId": "burger", "qty": 2 } ]
}
```

| Field          | Required for                          | Notes |
|-----------------|----------------------------------------|-------|
| `eventId`       | always                                 | Producer-generated UUID. On `order.create`, the service derives the order's ID from this: `orderId = UUIDv5(ORDER_NAMESPACE, eventId)` — so replaying the exact same create event is a safe no-op, not a duplicate order. |
| `eventType`     | always                                 | `order.create` \| `order.update.status` \| `order.update.items` |
| `occurredAt`    | always                                 | RFC3339 timestamp — the **ordering key**. Determines "latest," not when the message happened to arrive. |
| `orderId`       | updates only                           | Must reference a real order for normal generator traffic; absent/empty on `order.create` (the service assigns it). |
| `customerId`    | `order.create` only                    | Omitted on updates. |
| `restaurantId`  | `order.create` only                    | Omitted on updates. |
| `status`        | `order.update.status` only             | One of the four enum values. |
| `items`         | `order.create`, `order.update.items`   | `[{ "itemId": string, "qty": int }]` |

See `DESIGN.md` for the full correctness reasoning (idempotency, ordering,
concurrency, state-machine enforcement) built on top of this envelope.
