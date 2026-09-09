CREATE TABLE blob_cleanup_tasks (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    storage_key text NOT NULL,
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT blob_cleanup_tasks_storage_key_not_empty CHECK (storage_key <> ''),
    CONSTRAINT blob_cleanup_tasks_attempts_nonnegative CHECK (attempts >= 0),
    CONSTRAINT blob_cleanup_tasks_storage_key_unique UNIQUE (storage_key)
);

CREATE INDEX blob_cleanup_tasks_due_idx
    ON blob_cleanup_tasks (next_attempt_at, id);
