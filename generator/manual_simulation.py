"""Manual controlled simulation for demo/testing.

Publishes a pre-planned sequence of events to NATS JetStream step-by-step
with pauses, so each state change can be observed via the dashboard or API.

Usage:
    # Ensure infra + service are running
    docker compose up -d
    go run main.go

    # Reset DB for clean state
    docker exec -it food_delivery_db psql -U vaurd_user -d food_delivery -c "TRUNCATE TABLE orders;"

    # Run simulation
    python generator/manual_simulation.py
"""

import asyncio
import json
import os
import time
import uuid as uuid_mod
from datetime import datetime, timezone

import nats

NATS_URL = os.getenv("NATS_URL", "nats://localhost:4222")
SUBJECT = "orders.events"
API_BASE_URL = os.getenv("API_BASE_URL", "http://localhost:8080")

# Must match broker/consumer.go OrderNamespace
ORDER_NAMESPACE = uuid_mod.UUID("258b0800-4e9e-43ee-b246-94ff45ee14b6")
PAUSE_SECONDS = 3


def now_iso():
    return datetime.now(timezone.utc).isoformat()


def order_id_from_event(event_id: str) -> str:
    return str(uuid_mod.uuid5(ORDER_NAMESPACE, event_id))


async def publish(js, event, label=""):
    await js.publish(SUBJECT, json.dumps(event).encode())
    print(f"  ['{label or event['eventType']}")
    print(f"     {json.dumps(event, indent=2)}")
    print()


async def pause(seconds=PAUSE_SECONDS):
    print(f"  ... waiting {seconds}s (check dashboard or curl {API_BASE_URL}/orders)")
    await asyncio.sleep(seconds)
    print()


async def section(title):
    print("=" * 70)
    print(f"  {title}")
    print("=" * 70)
    print()


async def main():
    nc = await nats.connect(NATS_URL)
    js = nc.jetstream()
    start = time.time()
    print(f"Connected to NATS at {NATS_URL}")
    print()

    # =========================================================================
    # SCENARIO 1: Happy Path — Normal Order Lifecycle
    # =========================================================================
    await section("SCENARIO 1: Happy Path -- Order-A: Received -> Preparing -> Complete")

    eid_1 = "s1-create"
    event = {
        "eventId": eid_1,
        "eventType": "order.create",
        "occurredAt": now_iso(),
        "customerId": "customer-alice",
        "restaurantId": "restaurant-pizzeria",
        "items": [{"itemId": "margherita", "qty": 2}, {"itemId": "coke", "qty": 1}],
    }
    await publish(js, event, "create: Alice orders 2x Margherita + 1x Coke")
    oid_1 = order_id_from_event(eid_1)
    print(f"     -> Order ID: {oid_1}")
    print(f"     -> Expect: status=Received, items=margherita x2, coke x1")
    print(f"     -> curl '{API_BASE_URL}/orders?sort=last_updated_desc&limit=5'")
    await pause()

    event = {
        "eventId": "s1-prepare",
        "eventType": "order.update.status",
        "occurredAt": now_iso(),
        "orderId": oid_1,
        "status": "Preparing",
    }
    await publish(js, event, "status: Preparing")
    print(f"     -> Expect: status=Preparing")
    await pause()

    event = {
        "eventId": "s1-complete",
        "eventType": "order.update.status",
        "occurredAt": now_iso(),
        "orderId": oid_1,
        "status": "Complete",
    }
    await publish(js, event, "status: Complete")
    print(f"     -> Expect: status=Complete")
    await pause()

    # =========================================================================
    # SCENARIO 2: Cancellation from non-terminal state
    # =========================================================================
    await section("SCENARIO 2: Cancellation -- Order-B: Received -> Cancelled")

    eid_2 = "s2-create"
    event = {
        "eventId": eid_2,
        "eventType": "order.create",
        "occurredAt": now_iso(),
        "customerId": "customer-bob",
        "restaurantId": "restaurant-burger-shop",
        "items": [{"itemId": "cheeseburger", "qty": 1}],
    }
    await publish(js, event, "create: Bob orders 1x Cheeseburger")
    oid_2 = order_id_from_event(eid_2)
    print(f"     -> Order ID: {oid_2}")
    print(f"     -> Expect: status=Received")
    await pause()

    event = {
        "eventId": "s2-cancel",
        "eventType": "order.update.status",
        "occurredAt": now_iso(),
        "orderId": oid_2,
        "status": "Cancelled",
    }
    await publish(js, event, "status: Cancelled")
    print(f"     -> Expect: status=Cancelled (valid from Received)")
    await pause()

    # =========================================================================
    # SCENARIO 3: Items Replacement
    # =========================================================================
    await section("SCENARIO 3: Items Update -- Order-C: items replaced (not merged)")

    eid_3 = "s3-create"
    event = {
        "eventId": eid_3,
        "eventType": "order.create",
        "occurredAt": now_iso(),
        "customerId": "customer-charlie",
        "restaurantId": "restaurant-sushi-bar",
        "items": [{"itemId": "salmon-roll", "qty": 3}],
    }
    await publish(js, event, "create: Charlie orders 3x Salmon Roll")
    oid_3 = order_id_from_event(eid_3)
    print(f"     -> Order ID: {oid_3}")
    print(f"     -> Expect: items=salmon-roll x3")
    await pause()

    event = {
        "eventId": "s3-update-items",
        "eventType": "order.update.items",
        "occurredAt": now_iso(),
        "orderId": oid_3,
        "items": [{"itemId": "ramen", "qty": 1}, {"itemId": "gyoza", "qty": 2}],
    }
    await publish(js, event, "update.items: ramen x1, gyoza x2")
    print(f"     -> Expect: items=ramen x1, gyoza x2 (REPLACED, not appended)")
    await pause()

    # =========================================================================
    # SCENARIO 4: Invalid Status Transition
    # =========================================================================
    await section("SCENARIO 4: Invalid Transition -- Order-D: Complete -> Preparing (REJECTED)")

    eid_4 = "s4-create"
    event = {
        "eventId": eid_4,
        "eventType": "order.create",
        "occurredAt": now_iso(),
        "customerId": "customer-diana",
        "restaurantId": "restaurant-salad-stop",
        "items": [{"itemId": "caesar-salad", "qty": 1}],
    }
    await publish(js, event, "create: Diana orders 1x Caesar Salad")
    oid_4 = order_id_from_event(eid_4)
    print(f"     -> Order ID: {oid_4}")
    await pause()

    event = {
        "eventId": "s4-complete",
        "eventType": "order.update.status",
        "occurredAt": now_iso(),
        "orderId": oid_4,
        "status": "Complete",
    }
    await publish(js, event, "status: Complete")
    print(f"     -> Expect: status=Complete")
    await pause()

    event = {
        "eventId": "s4-invalid",
        "eventType": "order.update.status",
        "occurredAt": now_iso(),
        "orderId": oid_4,
        "status": "Preparing",
    }
    await publish(js, event, "status: Preparing (INVALID transition!)")
    print(f"     -> Expect: status STAYS Complete (state machine rejects Preparing)")
    print(f"     -> Check service logs for 'invalid status transition rejected'")
    await pause()

    # =========================================================================
    # SCENARIO 5: Update-Before-Create
    # =========================================================================
    await section("SCENARIO 5: Update-Before-Create -- Order-E: partial row then items fill")

    create_eid_5 = "s5-create"
    oid_5 = order_id_from_event(create_eid_5)
    print(f"     -> Pre-computed order_id = uuid5(namespace, '{create_eid_5}') = {oid_5}")
    print()

    event = {
        "eventId": "s5-update-first",
        "eventType": "order.update.status",
        "occurredAt": now_iso(),
        "orderId": oid_5,
        "status": "Preparing",
    }
    await publish(js, event, "status update BEFORE any create (update-before-create)")
    print(f"     -> Expect: PARTIAL row created, status=Preparing, customerId=null, restaurantId=null")
    print(f"     -> The service does NOT crash -- it creates a row on the fly (plan.md section 3.7)")
    await pause()

    event = {
        "eventId": "s5-update-items",
        "eventType": "order.update.items",
        "occurredAt": now_iso(),
        "orderId": oid_5,
        "items": [{"itemId": "taco", "qty": 3}],
    }
    await publish(js, event, "update.items on the partial row (status=NULL -> bypasses state machine)")
    print(f"     -> Expect: items=taco x3 set on the partial row, status stays Preparing")
    print(f"     -> Items updates have no status, so EXCLUDED.status IS NULL passes the WHERE clause")
    await pause()

    event = {
        "eventId": create_eid_5,
        "eventType": "order.create",
        "occurredAt": now_iso(),
        "customerId": "customer-eve",
        "restaurantId": "restaurant-taco-stand",
        "items": [{"itemId": "taco", "qty": 3}],
    }
    await publish(js, event, "create arrives late (gracefully handled as no-op)")
    print(f"     -> Expect: no change (state machine blocks Preparing -> Received)")
    print(f"     -> The system does NOT error or crash -- event is silently ignored (correct)")
    print(f"     -> Note: customer/restaurant stay null because the create's status transition is blocked")
    await pause()

    # =========================================================================
    # SCENARIO 6: Duplicate + Out-of-Order (LWW)
    # =========================================================================
    await section("SCENARIO 6: Duplicate + Stale -- Order-F: idempotency + LWW rejection")

    eid_6 = "s6-create"
    event = {
        "eventId": eid_6,
        "eventType": "order.create",
        "occurredAt": now_iso(),
        "customerId": "customer-frank",
        "restaurantId": "restaurant-noodle-house",
        "items": [{"itemId": "pad-thai", "qty": 1}],
    }
    await publish(js, event, "create: Frank orders 1x Pad Thai")
    oid_6 = order_id_from_event(eid_6)
    print(f"     -> Order ID: {oid_6}")
    print(f"     -> Expect: status=Received")
    await pause()

    await publish(js, event, "REPLAY: exact same create event (same eventId)")
    print(f"     -> Expect: NO change (PK conflict -> no-op, deterministic UUIDv5)")
    print(f"     -> Verify: total order count stays the same")
    await pause()

    event = {
        "eventId": "s6-prepare",
        "eventType": "order.update.status",
        "occurredAt": now_iso(),
        "orderId": oid_6,
        "status": "Preparing",
    }
    await publish(js, event, "status: Preparing")
    print(f"     -> Expect: status=Preparing")
    await pause()

    stale_time = "2024-01-01T00:00:00+00:00"
    event = {
        "eventId": "s6-stale",
        "eventType": "order.update.status",
        "occurredAt": stale_time,
        "orderId": oid_6,
        "status": "Complete",
    }
    await publish(js, event, f"STALE: backdated status update (occurredAt={stale_time})")
    print(f"     -> Expect: status STAYS Preparing (LWW: stale timestamp rejected)")
    print(f"     -> Verify: the event's occurredAt is older than last_event_at")
    await pause()

    # =========================================================================
    # SUMMARY
    # =========================================================================
    elapsed = time.time() - start
    await section(f"SIMULATION COMPLETE ({elapsed:.0f}s)")

    print(f"  See all orders:")
    print(f"    curl '{API_BASE_URL}/orders?limit=10' | python -m json.tool")
    print()
    print(f"  Or open the dashboard: http://localhost:8501")
    print()
    print("  Orders created:")
    print(f"    Order-A (Happy Path):      {oid_1}")
    print(f"    Order-B (Cancellation):    {oid_2}")
    print(f"    Order-C (Items Update):    {oid_3}")
    print(f"    Order-D (Invalid Trans):   {oid_4}")
    print(f"    Order-E (Upd-before-Cr):   {oid_5}")
    print(f"    Order-F (Dedup + LWW):     {oid_6}")

    await nc.close()


if __name__ == "__main__":
    asyncio.run(main())
