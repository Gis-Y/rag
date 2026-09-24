-- Required document tables. New databases use docs/ddl.sql; this script can
-- provision the same tables in an existing development database.
CREATE TABLE IF NOT EXISTS document_processing_states (
    document_id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
    user_id BIGINT UNSIGNED NOT NULL,
    file_md5 VARCHAR(32) NOT NULL,
    active_version VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    pending_version VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    status VARCHAR(24) NOT NULL,
    error TEXT,
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Published and pending immutable document generations';

CREATE TABLE IF NOT EXISTS document_chunks (
    document_id BIGINT UNSIGNED NOT NULL,
    version VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    chunk_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    parent_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    user_id BIGINT UNSIGNED NOT NULL,
    is_parent BOOLEAN NOT NULL,
    data JSON NOT NULL,
    created_at DATETIME(6) NOT NULL,
    PRIMARY KEY (document_id, version, chunk_id),
    KEY idx_document_parent (document_id, version, parent_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Structure-aware parent and child chunks with provenance';
