-- Apply once to a development database before starting this version.
-- No uploaded-object migration is provided: the project uses the new mode only.
-- Existing completed documents need re-uploading into the new immutable namespace.
ALTER TABLE file_upload
    ADD COLUMN merge_token VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    ADD COLUMN merge_expires_at DATETIME(6) NULL,
    MODIFY COLUMN merged_at TIMESTAMP NULL DEFAULT NULL COMMENT '合并时间';

ALTER TABLE chunk_info
    ADD COLUMN document_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
    ADD COLUMN user_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
    ADD INDEX idx_chunk_document (document_id);

CREATE TABLE IF NOT EXISTS document_task_outbox (
    document_id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
    user_id BIGINT UNSIGNED NOT NULL,
    attempts INT NOT NULL DEFAULT 0,
    available_at DATETIME(6) NOT NULL,
    lease_token VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    lease_until DATETIME(6) NULL,
    last_error VARCHAR(1000) NOT NULL DEFAULT '',
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    KEY idx_document_task_available (available_at, lease_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Durable at-least-once document task delivery';
