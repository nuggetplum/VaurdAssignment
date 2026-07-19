"""Demo/test hook for plan.md §3.7 (update-before-create).

Publishes ONE raw order.update.status event straight to JetStream for an
order ID that has never been created -- bypassing generate.py's discovery
cache entirely. Proves the service's defensive merge: it should insert a
partial row (customerId/restaurantId = null) instead of dropping the event
or erroring.

Usage:
    python generator/demo_hook.py
Then check:
    GET /orders?status=Preparing
and look for the printed order ID with null customer/restaurant fields.
"""

import asyncio
import json
import os
import uuid
from datetime import datetime, timezone

import nats

NATS_URL = os.getenv("NATS_URL", "nats://localhost:4222")
SUBJECT = "orders.events"


async def main():
    nc = await nats.connect(NATS_URL)
    js = nc.jetstream()

    order_id = str(uuid.uuid4())
    event = {
        "eventId": str(uuid.uuid4()),
        "eventType": "order.update.status",
        "occurredAt": datetime.now(timezone.utc).isoformat(),
        "orderId": order_id,
        "status": "Preparing",
    }
    await js.publish(SUBJECT, json.dumps(event).encode())
    await nc.close()

    print(f"Published a raw status-update for a NEVER-CREATED order: {order_id}")
    print("This bypasses the generator's discovery cache entirely (plan.md section 3.7).")
    print(f"Check GET /orders?status=Preparing -- you should see order {order_id}")
    print("with customerId/restaurantId = null, proving the update-before-create merge works.")


if __name__ == "__main__":
    asyncio.run(main())
