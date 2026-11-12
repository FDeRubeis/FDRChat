-- Applied on every start. Everything is IF NOT EXISTS, so an existing database
-- is left as it is: changing a table here does not alter one that was already
-- created. That is a deliberate limit, see the README.

CREATE TABLE IF NOT EXISTS users (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text        NOT NULL,
    password_hash text        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS users_name_lower_idx ON users (lower(name));

CREATE TABLE IF NOT EXISTS sessions (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash   bytea       NOT NULL UNIQUE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS sessions_user_id_idx ON sessions (user_id);

CREATE TABLE IF NOT EXISTS messages (
    -- v7 keeps primary key inserts append-only in the index, unlike random v4
    -- ids. It is not a clock: it is only monotonic per connection, so order
    -- by created_at and use id as the tie-breaker.
    id           uuid        PRIMARY KEY DEFAULT uuidv7(),
    sender_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    recipient_id uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    body         text        NOT NULL,
    status       text        NOT NULL DEFAULT 'saved',
    created_at   timestamptz NOT NULL DEFAULT now(),
    delivered_at timestamptz,
    read_at      timestamptz,

    CONSTRAINT messages_not_to_self CHECK (sender_id <> recipient_id),
    -- char_length counts code points, matching utf8.RuneCountInString in the handler.
    CONSTRAINT messages_body_length CHECK (char_length(body) BETWEEN 1 AND 2000),
    -- The status only moves forward, so the timestamps pin down which one is current.
    CONSTRAINT messages_status_timestamps CHECK (
        (status = 'saved'     AND delivered_at IS NULL     AND read_at IS NULL) OR
        (status = 'delivered' AND delivered_at IS NOT NULL AND read_at IS NULL) OR
        (status = 'read'      AND delivered_at IS NOT NULL AND read_at IS NOT NULL)
    )
);

-- A conversation is read in both directions, so both orderings are indexed.
-- (created_at, id) is the keyset the history is paged by.
CREATE INDEX IF NOT EXISTS messages_sender_recipient_idx ON messages (sender_id, recipient_id, created_at, id);
CREATE INDEX IF NOT EXISTS messages_recipient_sender_idx ON messages (recipient_id, sender_id, created_at, id);
