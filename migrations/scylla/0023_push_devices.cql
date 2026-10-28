CREATE TABLE IF NOT EXISTS push_devices (
    user_id uuid,
    session_hash blob,
    platform text,
    device_token text,
    sandbox boolean,
    created_at timestamp,
    PRIMARY KEY (user_id, session_hash)
);

CREATE TABLE IF NOT EXISTS push_tokens (
    platform text,
    device_token text,
    user_id uuid,
    session_hash blob,
    PRIMARY KEY ((platform, device_token))
);
