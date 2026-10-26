ALTER TABLE users ADD bot boolean;
ALTER TABLE users ADD bot_owner uuid;

CREATE TABLE IF NOT EXISTS bots (
    id uuid PRIMARY KEY,
    owner_id uuid,
    description text,
    sees_all boolean,
    commands blob,
    token_hash blob,
    created_at timestamp,
    webhook_url text,
    webhook_secret text,
    delivered_pts bigint,
    webhook_error text,
    webhook_error_at timestamp
);

CREATE TABLE IF NOT EXISTS user_bots (
    owner_id uuid,
    bot_id uuid,
    PRIMARY KEY (owner_id, bot_id)
);

CREATE TABLE IF NOT EXISTS bot_tokens (
    token_hash blob PRIMARY KEY,
    bot_id uuid
);

ALTER TABLE channel_members ADD bot boolean;
ALTER TABLE channel_members ADD bot_sees_all boolean;

ALTER TABLE messages ADD markup blob;
ALTER TABLE replies ADD markup blob;

CREATE TABLE IF NOT EXISTS channel_state (
    channel_id uuid PRIMARY KEY,
    pts bigint
);

CREATE TABLE IF NOT EXISTS channel_updates (
    channel_id uuid,
    bucket bigint,
    pts bigint,
    kind int,
    message_id timeuuid,
    root_id timeuuid,
    event blob,
    PRIMARY KEY ((channel_id, bucket), pts)
) WITH CLUSTERING ORDER BY (pts ASC)
    AND compaction = {'class': 'TimeWindowCompactionStrategy', 'compaction_window_unit': 'DAYS', 'compaction_window_size': 1};

CREATE TABLE IF NOT EXISTS account_state (
    user_id uuid PRIMARY KEY,
    pts bigint
);

CREATE TABLE IF NOT EXISTS account_updates (
    user_id uuid,
    bucket bigint,
    pts bigint,
    kind int,
    channel_id uuid,
    message_id timeuuid,
    root_id timeuuid,
    event blob,
    PRIMARY KEY ((user_id, bucket), pts)
) WITH CLUSTERING ORDER BY (pts ASC)
    AND compaction = {'class': 'TimeWindowCompactionStrategy', 'compaction_window_unit': 'DAYS', 'compaction_window_size': 1};
