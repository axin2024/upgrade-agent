// Package records persists scoped upgrade workspaces and their evidence in MySQL.
package records

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	mysql "github.com/go-sql-driver/mysql"
	"github.com/golang-migrate/migrate/v4"
	migratemysql "github.com/golang-migrate/migrate/v4/database/mysql"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/axin2024/upgrade-agent/internal/snapshot"
)

//go:embed migrations/*.up.sql
var migrations embed.FS

var (
	ErrMissing  = errors.New("object not found")
	ErrDenied   = errors.New("permission denied")
	ErrConflict = errors.New("request conflicts with current workspace state")
	ErrInvalid  = errors.New("invalid input")
	identifier  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)
	requestID   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:-]{0,95}$`)
	digest      = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type Store struct{ db *sql.DB }

func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("invalid MySQL configuration")
	}
	cfg.ParseTime, cfg.Loc, cfg.MultiStatements = true, time.UTC, false
	cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout = 5*time.Second, 20*time.Second, 20*time.Second
	if cfg.Params == nil {
		cfg.Params = make(map[string]string)
	}
	cfg.Params["time_zone"] = "'+00:00'"
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(4 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ready(ctx context.Context) error {
	var version int
	var dirty bool
	if err := s.db.QueryRowContext(ctx, "SELECT version,dirty FROM schema_migrations").Scan(&version, &dirty); err != nil {
		return err
	}
	if version != 4 || dirty {
		return errors.New("workspace schema requires a completed migration to version 4")
	}
	return nil
}

func Migrate(dsn string, steps int) error {
	config, err := mysql.ParseDSN(dsn)
	if err != nil {
		return fmt.Errorf("invalid MySQL configuration")
	}
	config.MultiStatements = true
	db, err := sql.Open("mysql", config.FormatDSN())
	if err != nil {
		return err
	}
	defer db.Close()
	source, err := iofs.New(migrations, "migrations")
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := migratemysql.WithInstance(db, &migratemysql.Config{})
	if err != nil {
		return err
	}
	engine, err := migrate.NewWithInstance("iofs", source, "mysql", target)
	if err != nil {
		return err
	}
	if steps > 0 {
		err = engine.Steps(steps)
	} else {
		err = engine.Up()
	}
	if errors.Is(err, migrate.ErrNoChange) {
		return nil
	}
	return err
}

type Identity struct {
	Tenant    string `json:"tenant"`
	Workspace string `json:"workspace"`
	Actor     string `json:"actor"`
	Role      string `json:"role"`
	secret    [32]byte
}

func (i Identity) Can(roles ...string) bool {
	return i.Role == "admin" || slices.Contains(roles, i.Role)
}

type Grant struct {
	Identity
	Token   string    `json:"token"`
	Expires time.Time `json:"expires"`
}

func (s *Store) Provision(ctx context.Context, tenant, workspace, actor, role string, duration time.Duration) (Grant, error) {
	if !identifier.MatchString(tenant) || !identifier.MatchString(workspace) || !identifier.MatchString(actor) ||
		!slices.Contains([]string{"admin", "maintainer", "reviewer", "verifier"}, role) ||
		duration < time.Minute || duration > 31*24*time.Hour {
		return Grant{}, ErrInvalid
	}
	return transaction(ctx, s, func(tx *sql.Tx) (Grant, error) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO workspaces(tenant,name) VALUES(?,?)
			ON DUPLICATE KEY UPDATE name=name`, tenant, workspace); err != nil {
			return Grant{}, err
		}
		grant := Grant{Identity: Identity{Tenant: tenant, Workspace: workspace, Actor: actor, Role: role},
			Token: hex.EncodeToString(randBytes(32)), Expires: time.Now().UTC().Add(duration)}
		tokenHash := sha256.Sum256([]byte(grant.Token))
		_, err := tx.ExecContext(ctx, `INSERT INTO access_grants(secret,tenant,workspace,actor,role,expires)
			VALUES(?,?,?,?,?,?)`, tokenHash[:], tenant, workspace, actor, role, grant.Expires)
		return grant, err
	})
}

func randBytes(n int) []byte {
	value := make([]byte, n)
	rand.Read(value)
	return value
}

func (s *Store) Identify(ctx context.Context, token string) (Identity, error) {
	if !digest.MatchString(token) {
		return Identity{}, ErrDenied
	}
	sum := sha256.Sum256([]byte(token))
	i := Identity{secret: sum}
	err := s.db.QueryRowContext(ctx, `SELECT tenant,workspace,actor,role FROM access_grants
		WHERE secret=? AND expires>UTC_TIMESTAMP(6) AND revoked IS NULL`, sum[:]).Scan(
		&i.Tenant, &i.Workspace, &i.Actor, &i.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return Identity{}, ErrDenied
	}
	return i, err
}

func (s *Store) Revoke(ctx context.Context, tenant, workspace, actor string) (int64, error) {
	if !identifier.MatchString(tenant) || !identifier.MatchString(workspace) || !identifier.MatchString(actor) {
		return 0, ErrInvalid
	}
	return transaction(ctx, s, func(tx *sql.Tx) (int64, error) {
		i := Identity{Tenant: tenant, Workspace: workspace}
		if _, err := current(ctx, tx, i, true); err != nil {
			return 0, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE access_grants SET revoked=UTC_TIMESTAMP(6)
			WHERE tenant=? AND workspace=? AND actor=? AND revoked IS NULL`, tenant, workspace, actor)
		if err != nil {
			return 0, err
		}
		return result.RowsAffected()
	})
}

type Workspace struct {
	Baseline string `json:"baseline"`
	Revision uint64 `json:"revision"`
}

type rowScanner interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func current(ctx context.Context, db rowScanner, i Identity, lock bool) (Workspace, error) {
	var value Workspace
	query := "SELECT COALESCE(snapshot,''),revision FROM workspaces WHERE tenant=? AND name=?"
	if lock {
		query += " FOR UPDATE"
	}
	err := db.QueryRowContext(ctx, query, i.Tenant, i.Workspace).Scan(&value.Baseline, &value.Revision)
	return value, missing(err)
}

func (s *Store) Current(ctx context.Context, i Identity) (Workspace, error) {
	return current(ctx, s.db, i, false)
}

func missing(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrMissing
	}
	return err
}

func transaction[T any](ctx context.Context, s *Store, action func(*sql.Tx) (T, error)) (T, error) {
	var zero T
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	value, err := action(tx)
	if err != nil {
		return zero, err
	}
	if err := tx.Commit(); err != nil {
		return zero, err
	}
	return value, nil
}

func checkGrant(ctx context.Context, tx *sql.Tx, i Identity) error {
	var role string
	err := tx.QueryRowContext(ctx, `SELECT role FROM access_grants WHERE secret=? AND tenant=? AND workspace=?
		AND actor=? AND revoked IS NULL AND expires>UTC_TIMESTAMP(6)`, i.secret[:], i.Tenant, i.Workspace, i.Actor).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) || err == nil && role != i.Role {
		return ErrDenied
	}
	return err
}

func mutate[T any](ctx context.Context, s *Store, i Identity, key, action string, input any, roles []string,
	perform func(*sql.Tx, Workspace) (T, error)) (T, error) {
	var zero T
	if !i.Can(roles...) {
		return zero, ErrDenied
	}
	if !requestID.MatchString(key) {
		return zero, fmt.Errorf("%w: idempotency key is required", ErrInvalid)
	}
	body, err := json.Marshal(input)
	if err != nil {
		return zero, err
	}
	signature := snapshot.Digest(append(append([]byte(action), 0), body...))
	return transaction(ctx, s, func(tx *sql.Tx) (T, error) {
		ws, err := current(ctx, tx, i, true)
		if err != nil {
			return zero, err
		}
		if err := checkGrant(ctx, tx, i); err != nil {
			return zero, err
		}
		var fingerprint string
		var stored []byte
		err = tx.QueryRowContext(ctx, `SELECT request_digest,response FROM receipts
			WHERE tenant=? AND workspace=? AND actor=? AND request_id=?`, i.Tenant, i.Workspace, i.Actor, key).Scan(&fingerprint, &stored)
		if err == nil {
			if fingerprint != signature {
				return zero, ErrConflict
			}
			var response T
			err := json.Unmarshal(stored, &response)
			return response, err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return zero, err
		}
		result, err := perform(tx, ws)
		if err != nil {
			return zero, err
		}
		payload, err := json.Marshal(result)
		if err != nil {
			return zero, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO receipts(tenant,workspace,actor,request_id,request_digest,response)
			VALUES(?,?,?,?,?,?)`, i.Tenant, i.Workspace, i.Actor, key, signature, payload)
		return result, err
	})
}

func audit(ctx context.Context, tx *sql.Tx, i Identity, action, object string, detail any) error {
	payload, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_events(tenant,workspace,actor,action,object,detail)
		VALUES(?,?,?,?,?,?)`, i.Tenant, i.Workspace, i.Actor, action, object, payload)
	return err
}

func readJSON[T any](ctx context.Context, db rowScanner, i Identity, id, query string) (T, error) {
	var value T
	if !digest.MatchString(id) {
		return value, ErrMissing
	}
	var payload []byte
	if err := db.QueryRowContext(ctx, query, i.Tenant, i.Workspace, id).Scan(&payload); err != nil {
		return value, missing(err)
	}
	return value, json.Unmarshal(payload, &value)
}
