ALTER TABLE messages ADD link_preview blob;
ALTER TABLE messages ADD no_link_preview boolean;
ALTER TABLE replies ADD link_preview blob;
ALTER TABLE replies ADD no_link_preview boolean;

CREATE TABLE IF NOT EXISTS link_previews (
    url_hash blob PRIMARY KEY,
    preview blob
) WITH gc_grace_seconds = 86400;
