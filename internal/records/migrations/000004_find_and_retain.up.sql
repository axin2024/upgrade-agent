CREATE TABLE plan_targets (
    tenant VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    workspace VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    digest CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    package_name VARCHAR(160) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    version_digest BINARY(32) NOT NULL,
    PRIMARY KEY (tenant, workspace, digest, package_name),
    KEY target_history (tenant, workspace, package_name, version_digest, digest),
    FOREIGN KEY (tenant, workspace, digest) REFERENCES plans(tenant, workspace, digest)
) ENGINE=InnoDB;

INSERT INTO plan_targets(tenant, workspace, digest, package_name, version_digest)
SELECT p.tenant, p.workspace, p.digest, t.package_name,
    UNHEX(SHA2(JSON_UNQUOTE(JSON_EXTRACT(p.payload, CONCAT('$.targets.', JSON_QUOTE(t.package_name)))), 256))
FROM plans AS p,
    JSON_TABLE(COALESCE(JSON_KEYS(p.payload, '$.targets'), JSON_ARRAY()), '$[*]'
        COLUMNS (package_name VARCHAR(160) PATH '$')) AS t;

ALTER TABLE snapshots ADD KEY snapshot_retention (tenant, workspace, created_at, digest);
ALTER TABLE audit_events ADD KEY event_retention (tenant, workspace, happened_at, id);
