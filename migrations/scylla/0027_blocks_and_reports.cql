CREATE TABLE IF NOT EXISTS blocks (
    user_id uuid,
    blocked_id uuid,
    created_at timestamp,
    PRIMARY KEY (user_id, blocked_id)
);

CREATE TABLE IF NOT EXISTS reports (
    id timeuuid PRIMARY KEY,
    team_id uuid,
    reporter_id uuid,
    user_id uuid,
    channel_id uuid,
    message_id uuid,
    root_id uuid,
    excerpt text,
    reason text,
    details text,
    action text,
    resolved_by uuid,
    resolved_at timestamp
);

CREATE TABLE IF NOT EXISTS report_index (
    scope uuid,
    open boolean,
    id timeuuid,
    PRIMARY KEY ((scope, open), id)
) WITH CLUSTERING ORDER BY (id DESC);

INSERT INTO casbin_rules (ptype, v0, v1, v2, v3, v4, v5) VALUES ('p', 'system_user', 'report', 'create', '', '', '');
INSERT INTO casbin_rules (ptype, v0, v1, v2, v3, v4, v5) VALUES ('p', 'team_admin', 'report', 'read', '', '', '');
INSERT INTO casbin_rules (ptype, v0, v1, v2, v3, v4, v5) VALUES ('p', 'team_admin', 'report', 'resolve', '', '', '');
