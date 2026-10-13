-- Kleine JSON-documenten, zoals de toestand van de trader en de paper-exchange.
CREATE TABLE kv_store (
    key        VARCHAR(100) PRIMARY KEY,
    value      TEXT         NOT NULL,
    updated_at BIGINT       NOT NULL
);

CREATE TABLE positions (
    id             INTEGER PRIMARY KEY,
    product_id     VARCHAR(20) NOT NULL,
    opened_at      BIGINT      NOT NULL,
    closed_at      BIGINT,
    entry_quantity TEXT        NOT NULL,
    cost_price     TEXT        NOT NULL
);

CREATE TABLE orders (
    id              VARCHAR(100) PRIMARY KEY,
    client_order_id VARCHAR(100) NOT NULL UNIQUE,
    position_id     BIGINT REFERENCES positions (id),
    purpose         VARCHAR(20)  NOT NULL,
    product_id      VARCHAR(20)  NOT NULL,
    side            VARCHAR(4)   NOT NULL,
    price           TEXT         NOT NULL,
    size            TEXT         NOT NULL,
    filled_size     TEXT         NOT NULL,
    filled_value    TEXT         NOT NULL,
    fee             TEXT         NOT NULL,
    status          VARCHAR(20)  NOT NULL,
    created_at      BIGINT       NOT NULL,
    done_at         BIGINT
);
CREATE INDEX idx_orders_position ON orders (position_id);

CREATE TABLE fills (
    trade_id  VARCHAR(100) PRIMARY KEY,
    order_id  VARCHAR(100) NOT NULL REFERENCES orders (id),
    price     TEXT         NOT NULL,
    size      TEXT         NOT NULL,
    fee       TEXT         NOT NULL,
    liquidity VARCHAR(10)  NOT NULL,
    time      BIGINT       NOT NULL
);
CREATE INDEX idx_fills_order ON fills (order_id);

-- Audit log van alles wat de bot doet.
CREATE TABLE events (
    id      INTEGER PRIMARY KEY,
    at      BIGINT      NOT NULL,
    kind    VARCHAR(50) NOT NULL,
    message TEXT        NOT NULL
);
CREATE INDEX idx_events_at ON events (at);
