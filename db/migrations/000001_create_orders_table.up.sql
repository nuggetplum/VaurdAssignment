CREATE TABLE IF NOT EXISTS orders (
    order_id TEXT PRIMARY KEY,
    customer_id TEXT,
    restaurant_id TEXT,
    status TEXT DEFAULT 'Received',
    items JSONB DEFAULT '[]'::jsonb,
    last_event_at TIMESTAMPTZ NOT NULL,
    last_updated_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_orders_status ON orders(status);
CREATE INDEX IF NOT EXISTS idx_orders_last_updated ON orders(last_updated_at DESC);


