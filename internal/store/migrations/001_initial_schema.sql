-- Migration 001: initial NAVETTE Control Plane schema.

CREATE TABLE IF NOT EXISTS tasks (
    id           TEXT    NOT NULL PRIMARY KEY,
    goal         TEXT    NOT NULL,
    input        TEXT    NOT NULL DEFAULT '{}',   -- JSON
    constraints  TEXT    NOT NULL DEFAULT '{}',   -- JSON {timeout_ns,max_iterations,work_dir}
    state        TEXT    NOT NULL,
    created_at   TEXT    NOT NULL,                -- RFC3339Nano UTC
    updated_at   TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS runs (
    id            TEXT NOT NULL PRIMARY KEY,
    task_id       TEXT NOT NULL REFERENCES tasks(id),
    started_at    TEXT NOT NULL,
    ended_at      TEXT NOT NULL DEFAULT '',
    outcome       TEXT NOT NULL DEFAULT '',
    artifact_dir  TEXT NOT NULL DEFAULT '',   -- durable per-run artifact directory
    artifact_hash TEXT NOT NULL DEFAULT ''    -- content fingerprint of saved artifacts
);

CREATE TABLE IF NOT EXISTS events (
    id        INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    task_id   TEXT    NOT NULL REFERENCES tasks(id),
    run_id    TEXT    NOT NULL DEFAULT '',
    at        TEXT    NOT NULL,
    kind      TEXT    NOT NULL,
    from_state TEXT   NOT NULL DEFAULT '',
    to_state  TEXT    NOT NULL DEFAULT '',
    reason    TEXT    NOT NULL DEFAULT '',
    detail    TEXT    NOT NULL DEFAULT ''        -- JSON or empty
);

CREATE TABLE IF NOT EXISTS approvals (
    id        INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    task_id   TEXT    NOT NULL REFERENCES tasks(id),
    kind      TEXT    NOT NULL,                  -- pre_exec | result
    approved  INTEGER NOT NULL,                  -- 0/1
    decided_by TEXT   NOT NULL DEFAULT '',
    decided_at TEXT   NOT NULL,
    reason    TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_events_task ON events(task_id);
CREATE INDEX IF NOT EXISTS idx_approvals_task ON approvals(task_id);

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER NOT NULL PRIMARY KEY,
    applied_at TEXT    NOT NULL
);
