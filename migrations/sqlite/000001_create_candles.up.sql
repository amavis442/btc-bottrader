-- 1-minuut candles. start is een Unix-timestamp (seconden, UTC); bedragen als tekst voor exacte decimalen.
CREATE TABLE candles_1m (
    product_id VARCHAR(20) NOT NULL,
    start      BIGINT      NOT NULL,
    open       TEXT        NOT NULL,
    high       TEXT        NOT NULL,
    low        TEXT        NOT NULL,
    close      TEXT        NOT NULL,
    volume     TEXT        NOT NULL,
    PRIMARY KEY (product_id, start)
);
