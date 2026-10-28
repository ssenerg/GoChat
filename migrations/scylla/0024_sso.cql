CREATE TABLE IF NOT EXISTS sso_flows (
    state_hash blob PRIMARY KEY,
    provider text,
    nonce text,
    verifier blob,
    redirect_uri text,
    link_user uuid,
    app_challenge text,
    expires_at timestamp
);

CREATE TABLE IF NOT EXISTS sso_tickets (
    token_hash blob PRIMARY KEY,
    kind text,
    user_id uuid,
    challenge text,
    provider text,
    issuer text,
    subject text,
    email text,
    expires_at timestamp
);

CREATE TABLE IF NOT EXISTS sso_identities (
    issuer text,
    subject text,
    user_id uuid,
    PRIMARY KEY ((issuer, subject))
);

CREATE TABLE IF NOT EXISTS user_sso_identities (
    user_id uuid,
    issuer text,
    subject text,
    provider text,
    email text,
    linked_at timestamp,
    PRIMARY KEY (user_id, issuer, subject)
);

CREATE TABLE IF NOT EXISTS sso_issuer_users (
    issuer text,
    user_id uuid,
    subject text,
    provider text,
    email text,
    linked_at timestamp,
    PRIMARY KEY (issuer, user_id, subject)
);
