-- Apply after 20260903_conversation_memory.sql. Repeatable, no historical data removed.
CREATE TABLE IF NOT EXISTS user_memories (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    user_id BIGINT UNSIGNED NOT NULL,
    scope VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    kind VARCHAR(16) NOT NULL,
    `key` VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    content TEXT NOT NULL,
    keywords JSON NOT NULL,
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    expires_at DATETIME(6) DEFAULT NULL,
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    UNIQUE KEY uk_user_memory (user_id, scope, `key`),
    KEY idx_user_memory_expiry (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Explicitly confirmed user memories';
