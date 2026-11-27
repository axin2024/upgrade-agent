CREATE TABLE estate_repositories (
    tenant VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    workspace VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    id CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    package_name VARCHAR(160) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    origin VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    owner VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    policy JSON NOT NULL,
    policy_revision BIGINT UNSIGNED NOT NULL DEFAULT 0,
    snapshot CHAR(64) CHARACTER SET ascii COLLATE ascii_bin DEFAULT NULL,
    version_name VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    revision BIGINT UNSIGNED NOT NULL DEFAULT 1,
    PRIMARY KEY (tenant, workspace, id),
    KEY estate_packages (tenant, workspace, package_name, id),
    FOREIGN KEY (tenant, workspace) REFERENCES workspaces(tenant, name)
) ENGINE=InnoDB;

CREATE TABLE migration_campaigns (
    tenant VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    workspace VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    id CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    state VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    revision BIGINT UNSIGNED NOT NULL,
    payload JSON NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (tenant, workspace, id),
    FOREIGN KEY (tenant, workspace) REFERENCES workspaces(tenant, name)
) ENGINE=InnoDB;

CREATE TABLE campaign_candidates (
    tenant VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    workspace VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    campaign CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    id CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    wave_id VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    repository CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    actor VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_digest CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    state VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    deadline DATETIME(6) NOT NULL,
    payload JSON NOT NULL,
    PRIMARY KEY (tenant, workspace, campaign, id),
    KEY candidate_by_repo (tenant, workspace, campaign, wave_id, repository),
    FOREIGN KEY (tenant, workspace, campaign) REFERENCES migration_campaigns(tenant, workspace, id)
) ENGINE=InnoDB;

CREATE TABLE campaign_rehearsals (
    tenant VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    workspace VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    campaign CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    id CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    wave_id VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    payload JSON NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (tenant, workspace, campaign, id),
    FOREIGN KEY (tenant, workspace, campaign) REFERENCES migration_campaigns(tenant, workspace, id)
) ENGINE=InnoDB;

CREATE TABLE campaign_bundles (
    tenant VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    workspace VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    campaign CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    id CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    repository CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    wave_id VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    adoption_ref VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin DEFAULT NULL,
    payload JSON NOT NULL,
    PRIMARY KEY (tenant, workspace, campaign, id),
    KEY adoption_by_wave (tenant, workspace, campaign, wave_id, repository),
    FOREIGN KEY (tenant, workspace, campaign) REFERENCES migration_campaigns(tenant, workspace, id)
) ENGINE=InnoDB;

CREATE TABLE campaign_matrix_pairs (
    tenant VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    workspace VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    campaign CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    rehearsal CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    id CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    state VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    runner_profile VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    lease CHAR(32) CHARACTER SET ascii COLLATE ascii_bin DEFAULT NULL,
    lease_until DATETIME(6) DEFAULT NULL,
    queued_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    payload JSON NOT NULL,
    PRIMARY KEY (tenant, workspace, campaign, rehearsal, id),
    KEY rehearsal_queue (runner_profile, state, queued_at),
    FOREIGN KEY (tenant, workspace, campaign, rehearsal) REFERENCES campaign_rehearsals(tenant, workspace, campaign, id)
) ENGINE=InnoDB;

CREATE TABLE estate_snapshots (
    tenant VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    workspace VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    repository CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    digest CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    policy_revision BIGINT UNSIGNED NOT NULL,
    version_name VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    payload JSON NOT NULL,
    contract_catalog JSON NOT NULL,
    captured_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (tenant, workspace, repository, digest, policy_revision),
    FOREIGN KEY (tenant, workspace, repository) REFERENCES estate_repositories(tenant, workspace, id)
) ENGINE=InnoDB;
