CREATE TABLE users (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    login text NOT NULL,
    password_hash bytea NOT NULL,
    created_at timestamptz(0) NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT users_login_not_empty CHECK (login <> ''),
    CONSTRAINT users_login_unique UNIQUE (login)
);

CREATE TABLE sessions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash bytea NOT NULL,
    created_at timestamptz(0) NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz NULL,
    CONSTRAINT sessions_token_hash_unique UNIQUE (token_hash),
    CONSTRAINT sessions_expiry_after_creation CHECK (expires_at > created_at)
);

CREATE INDEX sessions_user_id_idx ON sessions (user_id);

CREATE TABLE documents (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    name text NOT NULL,
    mime text NULL,
    is_file boolean NOT NULL,
    is_public boolean NOT NULL,
    json_data jsonb NULL,
    storage_key text NULL,
    size_bytes bigint NULL,
    created_at timestamptz(0) NOT NULL DEFAULT CURRENT_TIMESTAMP,
    version bigint NOT NULL DEFAULT 1,
    CONSTRAINT documents_name_not_empty CHECK (name <> ''),
    CONSTRAINT documents_size_nonnegative CHECK (size_bytes IS NULL OR size_bytes >= 0),
    CONSTRAINT documents_version_positive CHECK (version > 0),
    CONSTRAINT documents_file_metadata CHECK (
        (is_file AND mime IS NOT NULL AND storage_key IS NOT NULL AND size_bytes IS NOT NULL)
        OR
        (NOT is_file AND storage_key IS NULL AND size_bytes IS NULL)
    )
);

CREATE INDEX documents_owner_id_idx ON documents (owner_id);
CREATE INDEX documents_list_order_idx ON documents (owner_id, name ASC, created_at DESC, id ASC);

CREATE FUNCTION enforce_document_version_increase() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.version <= OLD.version THEN
        RAISE EXCEPTION 'document version must increase' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER documents_version_increase
BEFORE UPDATE ON documents
FOR EACH ROW EXECUTE FUNCTION enforce_document_version_increase();

CREATE TABLE document_grants (
    document_id uuid NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (document_id, user_id)
);

CREATE INDEX document_grants_user_document_idx ON document_grants (user_id, document_id);
