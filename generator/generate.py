"""Continuous order-event generator (plan.md Phase 5).

Publishes a mixed stream of order.create / order.update.status /
order.update.items events to NATS JetStream, deliberately including
duplicates and out-of-order (backdated) updates so the Go service's
idempotency and last-write-wins handling can be observed live.

Config is entirely via env vars (all have sensible defaults):
  NATS_URL                  default nats://localhost:4222
  API_BASE_URL              default http://localhost:8080
  EVENT_INTERVAL_SECONDS    default 0.5   (rate: seconds between events)
  DISCOVERY_INTERVAL_SECONDS default 5    (how often to refresh known order IDs)
  DUPLICATE_PROBABILITY     default 0.1   (chance to replay the previous event)
  STALE_UPDATE_PROBABILITY default 0.15  (chance a status update is backdated)
"""

import asyncio
import json
import logging
import os
import random
import time
import uuid
from datetime import datetime, timedelta, timezone

import nats
import requests

NATS_URL = os.getenv("NATS_URL", "nats://localhost:4222")
API_BASE_URL = os.getenv("API_BASE_URL", "http://localhost:8080")
SUBJECT = "orders.events"

EVENT_INTERVAL_SECONDS = float(os.getenv("EVENT_INTERVAL_SECONDS", "0.5"))
DISCOVERY_INTERVAL_SECONDS = float(os.getenv("DISCOVERY_INTERVAL_SECONDS", "5"))
DUPLICATE_PROBABILITY = float(os.getenv("DUPLICATE_PROBABILITY", "0.1"))
STALE_UPDATE_PROBABILITY = float(os.getenv("STALE_UPDATE_PROBABILITY", "0.15"))

STATUSES = ["Received", "Preparing", "Complete", "Cancelled"]
CUSTOMERS = [f"customer-{i}" for i in range(1, 21)]
RESTAURANTS = [f"restaurant-{i}" for i in range(1, 11)]
MENU_ITEMS = ["burger", "pizza", "sushi", "taco", "salad", "fries", "soda", "noodles"]

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(message)s")
log = logging.getLogger("generator")


def random_items():
    chosen = random.sample(MENU_ITEMS, random.randint(1, 3))
    return [{"itemId": item, "qty": random.randint(1, 4)} for item in chosen]


def now_iso():
    return datetime.now(timezone.utc).isoformat()


class OrderIDCache:
    """Local cache of known order IDs, refreshed on a low cadence from
    GET /orders rather than on every event (plan.md D6)."""

    def __init__(self):
        self._ids = []
        self._last_refresh = 0.0

    def maybe_refresh(self):
        if time.time() - self._last_refresh < DISCOVERY_INTERVAL_SECONDS:
            return
        try:
            resp = requests.get(f"{API_BASE_URL}/orders", params={"limit": 200}, timeout=3)
            resp.raise_for_status()
            self._ids = [o["orderId"] for o in resp.json()["orders"]]
            self._last_refresh = time.time()
            log.info("refreshed order cache: %d known orders", len(self._ids))
        except requests.RequestException as err:
            log.warning("failed to refresh order cache: %s", err)

    def random_known_id(self):
        return random.choice(self._ids) if self._ids else None


def build_create_event():
    return {
        "eventId": str(uuid.uuid4()),
        "eventType": "order.create",
        "occurredAt": now_iso(),
        "customerId": random.choice(CUSTOMERS),
        "restaurantId": random.choice(RESTAURANTS),
        "items": random_items(),
    }


def build_status_update_event(order_id):
    occurred_at = now_iso()
    if random.random() < STALE_UPDATE_PROBABILITY:
        # Deliberately backdated: sent now, but claims to have happened
        # 5-60s ago. Demonstrates that ordering is by event time, not
        # arrival time -- this event should lose to anything newer that
        # already landed for the same order.
        occurred_at = (datetime.now(timezone.utc) - timedelta(seconds=random.randint(5, 60))).isoformat()
    return {
        "eventId": str(uuid.uuid4()),
        "eventType": "order.update.status",
        "occurredAt": occurred_at,
        "orderId": order_id,
        # Deliberately not following the valid state machine -- random
        # status choices will sometimes be invalid transitions, exercising
        # the service's D4 guard.
        "status": random.choice(STATUSES),
    }


def build_items_update_event(order_id):
    return {
        "eventId": str(uuid.uuid4()),
        "eventType": "order.update.items",
        "occurredAt": now_iso(),
        "orderId": order_id,
        "items": random_items(),
    }


async def publish(js, event):
    await js.publish(SUBJECT, json.dumps(event).encode())
    log.info("published %s %s (order=%s)", event["eventType"], event["eventId"], event.get("orderId", "<new>"))


async def connect_with_retry():
    delay = 1
    while True:
        try:
            return await nats.connect(NATS_URL)
        except Exception as err:  # noqa: BLE001 - just retrying transient connect failures
            log.warning("nats connect failed (%s), retrying in %ds", err, delay)
            await asyncio.sleep(delay)
            delay = min(delay * 2, 10)


async def main():
    nc = await connect_with_retry()
    js = nc.jetstream()
    log.info("connected to NATS at %s, publishing to subject %s", NATS_URL, SUBJECT)

    cache = OrderIDCache()
    last_event = None

    try:
        while True:
            cache.maybe_refresh()

            if last_event is not None and random.random() < DUPLICATE_PROBABILITY:
                event = last_event
            else:
                known_id = cache.random_known_id()
                if known_id is None:
                    event = build_create_event()
                else:
                    roll = random.random()
                    if roll < 0.34:
                        event = build_create_event()
                    elif roll < 0.67:
                        event = build_status_update_event(known_id)
                    else:
                        event = build_items_update_event(known_id)

            await publish(js, event)
            last_event = event
            await asyncio.sleep(EVENT_INTERVAL_SECONDS)
    finally:
        await nc.close()


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        log.info("generator stopped")
