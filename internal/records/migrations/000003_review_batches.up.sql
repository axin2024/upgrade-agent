CREATE TABLE review_batches (
    tenant VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    workspace VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    actor VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_id VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    payload JSON NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (tenant, workspace, actor, request_id),
    KEY recent_review_batches (tenant, workspace, actor, created_at DESC, request_id DESC),
    FOREIGN KEY (tenant, workspace) REFERENCES workspaces(tenant, name)
) ENGINE=InnoDB;
