CREATE TABLE IF NOT EXISTS users (
    id         text PRIMARY KEY,
    container  text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS artifacts (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id       text NOT NULL REFERENCES users (id),
    title          text NOT NULL,
    -- Pointer to the current version. The conflict check on publish relies on this column.
    live_version   integer NOT NULL,
    retention_days integer NOT NULL DEFAULT 30,
    expires_at     timestamptz NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    -- Tombstone: the blobs are gone, the row stays as a record.
    deleted_at     timestamptz,
    delete_reason  text
);

CREATE INDEX IF NOT EXISTS artifacts_owner ON artifacts (owner_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS artifacts_expiry ON artifacts (expires_at) WHERE deleted_at IS NULL;

-- Metadata only: the content of a version lives in the blobstore under <artifact-id>/v<number>/.
CREATE TABLE IF NOT EXISTS artifact_versions (
    artifact_id uuid NOT NULL REFERENCES artifacts (id),
    number      integer NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (artifact_id, number)
);
