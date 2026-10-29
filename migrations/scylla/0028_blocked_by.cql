CREATE TABLE IF NOT EXISTS blocked_by (
    blocked_id uuid,
    user_id uuid,
    PRIMARY KEY (blocked_id, user_id)
);

ALTER TABLE report_index ADD user_id uuid;
