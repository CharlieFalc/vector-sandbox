-- =============================================================================
-- Telemetry Router — Database Schema
-- =============================================================================
-- Designed for PostgreSQL 15+.
-- Every table uses UUID primary keys to avoid predictable enumeration and to
-- allow distributed generation without a central sequence.
--
-- Audit requirement: "Show me every event routed to our SIEM in the last 30
-- days and confirm none were dropped" must be answerable in < 1 second.
-- =============================================================================

-- ---------------------------------------------------------------------------
-- instances: one row per provisioned Telemetry Router instance
-- ---------------------------------------------------------------------------
CREATE TABLE instances (
    instance_id   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id    TEXT        NOT NULL,
    region        TEXT        NOT NULL,           -- e.g. 'eu-central-1'
    name          TEXT        NOT NULL,
    status        TEXT        NOT NULL DEFAULT 'PENDING',
                                                  -- PENDING | ACTIVE | FAILED | DELETING
    allow_non_eu  BOOLEAN     NOT NULL DEFAULT FALSE,
    replicas      INT         NOT NULL DEFAULT 1,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ---------------------------------------------------------------------------
-- destinations: one row per configured sink on a router instance
-- ---------------------------------------------------------------------------
CREATE TABLE destinations (
    destination_id  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    instance_id     UUID        NOT NULL REFERENCES instances(instance_id) ON DELETE CASCADE,
    name            TEXT        NOT NULL,
    type            TEXT        NOT NULL,          -- 'OTLP' | 'S3'
    status          TEXT        NOT NULL DEFAULT 'PENDING',
                                                   -- PENDING | ACTIVE | FAILED | DELETING
    -- Encrypted JSON blob holding endpoint, bucket, prefix, secret_ref, etc.
    -- Encrypted at rest using pgcrypto or application-level AES-256-GCM.
    config_encrypted BYTEA,

    -- Denormalised region for compliance queries without decrypting config.
    destination_region TEXT,                       -- e.g. 'eu-west-1' for S3 sinks
    allow_non_eu       BOOLEAN NOT NULL DEFAULT FALSE,

    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_destinations_instance ON destinations(instance_id);
CREATE INDEX idx_destinations_status   ON destinations(status);

-- ---------------------------------------------------------------------------
-- events: metadata record for each ingested OTLP batch
-- (individual log records within a batch are tracked in delivery_attempts)
-- ---------------------------------------------------------------------------
CREATE TABLE events (
    event_id     UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    instance_id  UUID        NOT NULL REFERENCES instances(instance_id),
    -- OTLP resource attributes stored as JSONB for flexible querying.
    resource     JSONB       NOT NULL DEFAULT '{}',
    -- Truncated log body for audit trail (full body stored in sink).
    body_preview TEXT,
    ingested_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Partition by ingested_at month for efficient 30-day retention queries.
-- In production: CREATE TABLE events_2026_03 PARTITION OF events
--                FOR VALUES FROM ('2026-03-01') TO ('2026-04-01');

CREATE INDEX idx_events_instance    ON events(instance_id);
CREATE INDEX idx_events_ingested_at ON events(ingested_at);

-- ---------------------------------------------------------------------------
-- delivery_attempts: the authoritative audit trail
--
-- One row per (event × destination × attempt_number).
-- This answers: "did event E reach destination D, and how many tries did it take?"
-- ---------------------------------------------------------------------------
CREATE TABLE delivery_attempts (
    attempt_id     UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id       UUID        NOT NULL REFERENCES events(event_id),
    destination_id UUID        NOT NULL REFERENCES destinations(destination_id),
    attempt_number INT         NOT NULL,           -- 1-indexed, max 5

    status         TEXT        NOT NULL,           -- 'SUCCESS' | 'FAILED' | 'IN_FLIGHT'
    error_message  TEXT,                           -- NULL on success
    duration_ms    INT,                            -- network round-trip time

    attempted_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    UNIQUE (event_id, destination_id, attempt_number)
);

CREATE INDEX idx_delivery_attempts_event       ON delivery_attempts(event_id);
CREATE INDEX idx_delivery_attempts_dest        ON delivery_attempts(destination_id);
CREATE INDEX idx_delivery_attempts_attempted   ON delivery_attempts(attempted_at);
-- Composite index supports the 30-day audit query efficiently.
CREATE INDEX idx_delivery_audit
    ON delivery_attempts(destination_id, attempted_at, status);

-- ---------------------------------------------------------------------------
-- async_tasks: tracks the state of long-running control-plane operations
-- (destination create / update / delete that require operator reconcile)
-- ---------------------------------------------------------------------------
CREATE TABLE async_tasks (
    task_id        UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    resource_type  TEXT        NOT NULL,          -- 'DESTINATION' | 'INSTANCE'
    resource_id    UUID        NOT NULL,
    action         TEXT        NOT NULL,          -- 'CREATE' | 'UPDATE' | 'DELETE'
    status         TEXT        NOT NULL DEFAULT 'PENDING',
                                                  -- PENDING | IN_PROGRESS | DONE | FAILED
    error_message  TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at   TIMESTAMPTZ
);

CREATE INDEX idx_async_tasks_resource ON async_tasks(resource_id);

-- =============================================================================
-- VIEWS
-- =============================================================================

-- ---------------------------------------------------------------------------
-- v_delivery_summary: latest delivery state per (event × destination)
-- Answers: "did this event reach each of its destinations?"
-- ---------------------------------------------------------------------------
CREATE OR REPLACE VIEW v_delivery_summary AS
SELECT
    da.event_id,
    da.destination_id,
    d.name                                              AS destination_name,
    d.type                                              AS destination_type,
    COUNT(*)                                            AS total_attempts,
    MAX(da.attempt_number)                              AS last_attempt_number,
    BOOL_OR(da.status = 'SUCCESS')                      AS delivered,
    MIN(CASE WHEN da.status = 'SUCCESS' THEN da.attempted_at END) AS first_success_at,
    MAX(CASE WHEN da.status = 'FAILED'  THEN da.error_message END) AS last_error
FROM delivery_attempts da
JOIN destinations d USING (destination_id)
GROUP BY da.event_id, da.destination_id, d.name, d.type;

-- ---------------------------------------------------------------------------
-- v_destination_health_1h: rolling 1-hour success/failure counts
-- Powers the GET /destinations/{id}/health endpoint without a separate store.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE VIEW v_destination_health_1h AS
SELECT
    destination_id,
    COUNT(*) FILTER (WHERE status = 'SUCCESS')          AS successes,
    COUNT(*) FILTER (WHERE status = 'FAILED')           AS failures,
    MAX(attempted_at) FILTER (WHERE status = 'SUCCESS') AS last_delivered_at,
    MAX(error_message) FILTER (WHERE status = 'FAILED') AS last_error
FROM delivery_attempts
WHERE attempted_at >= NOW() - INTERVAL '1 hour'
GROUP BY destination_id;

-- ---------------------------------------------------------------------------
-- fn_audit_destination_30d: stored function used by the customer audit team
--
-- Usage: SELECT * FROM fn_audit_destination_30d('<destination_id>');
-- Returns: one row per event with delivered boolean and drop indicator.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION fn_audit_destination_30d(p_destination_id UUID)
RETURNS TABLE (
    event_id        UUID,
    ingested_at     TIMESTAMPTZ,
    delivered       BOOLEAN,
    attempts        INT,
    first_success_at TIMESTAMPTZ,
    dropped         BOOLEAN       -- TRUE if all attempts failed (no success ever)
) LANGUAGE sql STABLE AS $$
    SELECT
        e.event_id,
        e.ingested_at,
        BOOL_OR(da.status = 'SUCCESS')                              AS delivered,
        COUNT(da.attempt_id)::INT                                   AS attempts,
        MIN(da.attempted_at) FILTER (WHERE da.status = 'SUCCESS')   AS first_success_at,
        NOT BOOL_OR(da.status = 'SUCCESS')                          AS dropped
    FROM events e
    LEFT JOIN delivery_attempts da
           ON da.event_id       = e.event_id
          AND da.destination_id = p_destination_id
    WHERE e.ingested_at >= NOW() - INTERVAL '30 days'
    GROUP BY e.event_id, e.ingested_at
    ORDER BY e.ingested_at DESC;
$$;

-- =============================================================================
-- COMPLIANCE: Row-level security ensures a project can only see its own data.
-- =============================================================================
ALTER TABLE instances     ENABLE ROW LEVEL SECURITY;
ALTER TABLE destinations  ENABLE ROW LEVEL SECURITY;
ALTER TABLE events        ENABLE ROW LEVEL SECURITY;
ALTER TABLE delivery_attempts ENABLE ROW LEVEL SECURITY;

-- Example policy — current_setting('app.project_id') is set per connection
-- by the connection pool (PgBouncer) using SET LOCAL before each query.
CREATE POLICY project_isolation ON instances
    USING (project_id = current_setting('app.project_id', TRUE));

CREATE POLICY project_isolation ON destinations
    USING (
        instance_id IN (
            SELECT instance_id FROM instances
            WHERE project_id = current_setting('app.project_id', TRUE)
        )
    );
