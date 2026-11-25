CREATE TABLE workspaces (
    tenant VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    name VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    snapshot CHAR(64) CHARACTER SET ascii COLLATE ascii_bin DEFAULT NULL,
    revision BIGINT UNSIGNED NOT NULL DEFAULT 0,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (tenant, name)
) ENGINE=InnoDB;

CREATE TABLE access_grants (
    secret BINARY(32) PRIMARY KEY,
    tenant VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    workspace VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    actor VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    role ENUM('admin','maintainer','reviewer','verifier') NOT NULL,
    expires DATETIME(6) NOT NULL,
    revoked DATETIME(6),
    KEY actor_grants (tenant, workspace, actor),
    FOREIGN KEY (tenant, workspace) REFERENCES workspaces(tenant, name)
) ENGINE=InnoDB;

CREATE TABLE snapshots (
    tenant VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    workspace VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    digest CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    payload JSON NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (tenant, workspace, digest),
    FOREIGN KEY (tenant, workspace) REFERENCES workspaces(tenant, name)
) ENGINE=InnoDB;

CREATE TABLE plans (
    tenant VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    workspace VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    digest CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    baseline CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    payload JSON NOT NULL,
    actor VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (tenant, workspace, digest),
    KEY plan_history (tenant, workspace, created_at, digest),
    FOREIGN KEY (tenant, workspace, baseline) REFERENCES snapshots(tenant, workspace, digest)
) ENGINE=InnoDB;

CREATE TABLE candidates (
    tenant VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    workspace VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    digest CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    plan CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    batch VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    payload JSON NOT NULL,
    actor VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (tenant, workspace, digest),
    KEY batch_history (tenant, workspace, plan, batch, created_at, digest),
    FOREIGN KEY (tenant, workspace, plan) REFERENCES plans(tenant, workspace, digest)
) ENGINE=InnoDB;

CREATE TABLE reports (
    tenant VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    workspace VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    digest CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    candidate CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    verdict ENUM('passed','blocked') NOT NULL,
    baseline_revision BIGINT UNSIGNED NOT NULL,
    payload JSON NOT NULL,
    actor VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (tenant, workspace, digest),
    KEY validation_history (tenant, workspace, candidate, created_at, digest),
    FOREIGN KEY (tenant, workspace, candidate) REFERENCES candidates(tenant, workspace, digest)
) ENGINE=InnoDB;

CREATE TABLE selections (
    tenant VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    workspace VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    plan CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    batch VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    candidate CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    evidence CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    PRIMARY KEY (tenant, workspace, plan, batch),
    FOREIGN KEY (tenant, workspace, plan) REFERENCES plans(tenant, workspace, digest),
    FOREIGN KEY (tenant, workspace, candidate) REFERENCES candidates(tenant, workspace, digest),
    FOREIGN KEY (tenant, workspace, evidence) REFERENCES reports(tenant, workspace, digest)
) ENGINE=InnoDB;

CREATE TABLE audit_events (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    tenant VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    workspace VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    actor VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    action VARCHAR(48) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    object CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    detail JSON NOT NULL,
    happened_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    KEY event_history (tenant, workspace, id),
    FOREIGN KEY (tenant, workspace) REFERENCES workspaces(tenant, name)
) ENGINE=InnoDB;

CREATE TABLE receipts (
    tenant VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    workspace VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    actor VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_id VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_digest CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    response JSON NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (tenant, workspace, actor, request_id),
    FOREIGN KEY (tenant, workspace) REFERENCES workspaces(tenant, name)
) ENGINE=InnoDB;
