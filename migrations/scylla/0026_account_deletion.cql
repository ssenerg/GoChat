ALTER TABLE users ADD deleted_at timestamp;

ALTER TABLE sso_flows ADD reauth boolean;
