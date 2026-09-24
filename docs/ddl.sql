CREATE TABLE users (
                       id BIGINT AUTO_INCREMENT PRIMARY KEY COMMENT '用户唯一标识',
                       username VARCHAR(255) NOT NULL UNIQUE COMMENT '用户名，唯一',
                       password VARCHAR(255) NOT NULL COMMENT '加密后的密码',
                       role ENUM('USER', 'ADMIN') NOT NULL DEFAULT 'USER' COMMENT '用户角色',
                       org_tags VARCHAR(255) DEFAULT NULL COMMENT '用户所属组织标签，多个用逗号分隔',
                       primary_org VARCHAR(50) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin DEFAULT NULL COMMENT '用户主组织标签',
                       created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
                       updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
                       INDEX idx_username (username) COMMENT '用户名索引'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='用户表';


CREATE TABLE organization_tags (
                                   tag_id VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin PRIMARY KEY COMMENT '标签唯一标识',
                                   name VARCHAR(100) NOT NULL COMMENT '标签名称',
                                   description TEXT COMMENT '描述',
                                   parent_tag VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin DEFAULT NULL COMMENT '父标签ID',
                                   created_by BIGINT NOT NULL COMMENT '创建者ID',
                                   created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
                                   updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
                                   FOREIGN KEY (parent_tag) REFERENCES organization_tags(tag_id) ON DELETE SET NULL,
                                   FOREIGN KEY (created_by) REFERENCES users(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='组织标签表';


CREATE TABLE file_upload (
                             id           BIGINT           NOT NULL AUTO_INCREMENT COMMENT '主键',
                             file_md5     VARCHAR(32)      NOT NULL COMMENT '文件 MD5',
                             file_name    VARCHAR(255)     NOT NULL COMMENT '文件名称',
                             total_size   BIGINT           NOT NULL COMMENT '文件大小',
                             status       TINYINT          NOT NULL DEFAULT 0 COMMENT '上传状态',
                             user_id      VARCHAR(64)      NOT NULL COMMENT '用户 ID',
                             org_tag      VARCHAR(50)      DEFAULT NULL COMMENT '组织标签',
                             is_public    TINYINT(1)       NOT NULL DEFAULT 0 COMMENT '是否公开',
                             created_at   TIMESTAMP        NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
                             merged_at    TIMESTAMP        NULL DEFAULT NULL COMMENT '合并时间',
                             merge_token VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
                             merge_expires_at DATETIME(6) NULL,
                             PRIMARY KEY (id),
                             UNIQUE KEY uk_md5_user (file_md5, user_id),
                             INDEX idx_user (user_id),
                             INDEX idx_org_tag (org_tag)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='文件上传记录';


CREATE TABLE chunk_info (
                            id BIGINT AUTO_INCREMENT PRIMARY KEY COMMENT '分块记录唯一标识',
                            document_id BIGINT UNSIGNED NOT NULL,
                            user_id BIGINT UNSIGNED NOT NULL,
                            file_md5 VARCHAR(32) NOT NULL COMMENT '关联的文件MD5值',
                            chunk_index INT NOT NULL COMMENT '分块序号',
                            chunk_md5 VARCHAR(32) NOT NULL COMMENT '分块的MD5值',
                            storage_path VARCHAR(255) NOT NULL COMMENT '分块在存储系统中的路径',
                            INDEX idx_chunk_document (document_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='文件分块信息表';


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
    sources JSON NULL,
    created_at DATETIME(6) NOT NULL,
    UNIQUE KEY uk_conversation_turn (conversation_id, turn_no),
    UNIQUE KEY uk_conversation_request (conversation_id, turn_id),
    KEY idx_conversation_user (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Immutable complete conversation turns';

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

-- Required state and parent/child storage for the document processing pipeline.
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

-- 不创建默认账号。首次部署请先通过注册接口创建用户，再按 README 将指定用户提升为管理员。
