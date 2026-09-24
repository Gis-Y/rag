-- This migration is repeatable for deployments using the former Redis-only chat storage.
-- If an independently created old conversations table already exists, inspect and back it up
-- before adding/backfilling the new identity columns; CREATE IF NOT EXISTS does not alter it.
-- Conversation memory: MySQL is the source of truth; Redis is a disposable recent-history cache.
-- Apply before starting the upgraded backend. No tables or historical rows are dropped.
CREATE TABLE IF NOT EXISTS conversation_states (
    id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY,
    user_id BIGINT UNSIGNED NOT NULL,
    version BIGINT UNSIGNED NOT NULL DEFAULT 0,
    last_turn BIGINT UNSIGNED NOT NULL DEFAULT 0,
    summary LONGTEXT NOT NULL,
    summary_until BIGINT UNSIGNED NOT NULL DEFAULT 0,
    context_after BIGINT UNSIGNED NOT NULL DEFAULT 0,
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    UNIQUE KEY uk_conversation_state_user (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Versioned ongoing conversation state';

CREATE TABLE IF NOT EXISTS conversations (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    user_id BIGINT UNSIGNED NOT NULL,
    conversation_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    turn_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    turn_no BIGINT UNSIGNED NOT NULL,
    question LONGTEXT NOT NULL,
    answer LONGTEXT NOT NULL,
    created_at DATETIME(6) NOT NULL,
    UNIQUE KEY uk_conversation_turn (conversation_id, turn_no),
    UNIQUE KEY uk_conversation_request (conversation_id, turn_id),
    KEY idx_conversation_user (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Immutable complete conversation turns';
