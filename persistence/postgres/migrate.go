package postgres

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed schema/*.sql
var schemaFS embed.FS

// Schema is the DDL for the tables this package owns — warren_outbox and
// warren_inbox — as numbered SQL files in goose's file format, so a project
// already running goose, atlas or dbmate applies them with one line.
//
// Warren never applies them for you. See Migrate.
var Schema fs.FS = mustSub(schemaFS, "schema")

// migrationsTable records what has been applied. It is this package's, not
// yours: a project running its own migration tool keeps its own table and
// simply applies Schema through that instead.
const migrationsTable = "warren_schema_migrations"

// migrateLockKey is a fixed advisory-lock key, distinct from the outbox's, so
// two runners started by two deploy jobs serialise instead of racing.
const migrateLockKey int64 = 5_478_213_442_901_155

// Migrate applies every unapplied file in fsys, in name order, each in its own
// transaction, under a Postgres advisory lock so concurrent runners
// serialise. Applied versions are recorded in warren_schema_migrations.
//
// It is called by your deploy job — and by `warren migrate` once the CLI
// grows that command. It is NEVER called
// from a lifecycle hook, and there is no option to make it one. Under a
// rolling deploy, migrating at boot races N replicas: N−1 block and get killed
// for missing their readiness deadline, the winner applies DDL that the still
// serving old replicas were not written against, and one bad file puts every
// replica into a crash loop at the same moment. Schema is a deploy step.
//
// There are no down migrations. A migration that must be undone is a
// migration you write forwards, because that is the one you can test.
func Migrate(ctx context.Context, dsn string, fsys fs.FS) error {
	if strings.TrimSpace(dsn) == "" {
		return errNoDSN()
	}
	names, err := migrationNames(fsys)
	if err != nil {
		return err
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return errCannotConnect(dsn, err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	// Serialise concurrent runners. Session-level, so a runner that dies
	// releases it by disconnecting — no lease, no clock, no stuck deploy.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateLockKey); err != nil {
		return fmt.Errorf("taking the migration lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrateLockKey)
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS `+quoteIdent(migrationsTable)+` (
			version    TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("creating %s: %w", migrationsTable, err)
	}

	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return err
	}

	for _, name := range names {
		if applied[name] {
			continue
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return fmt.Errorf("reading %s: %w", name, err)
		}
		stmt := upSection(string(body))
		if strings.TrimSpace(stmt) == "" {
			return errEmptyMigration(name)
		}
		// One transaction per file: a failure leaves the ones before it
		// applied and recorded, so re-running resumes rather than restarting.
		if err := applyOne(ctx, conn, name, stmt); err != nil {
			return err
		}
	}
	return nil
}

func applyOne(ctx context.Context, conn *pgx.Conn, name, stmt string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning %s: %w", name, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, stmt); err != nil {
		return errMigrationFailed(name, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO `+quoteIdent(migrationsTable)+` (version) VALUES ($1)`, name); err != nil {
		return fmt.Errorf("recording %s: %w", name, err)
	}
	return tx.Commit(ctx)
}

func appliedVersions(ctx context.Context, conn *pgx.Conn) (map[string]bool, error) {
	rows, err := conn.Query(ctx, `SELECT version FROM `+quoteIdent(migrationsTable))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", migrationsTable, err)
	}
	defer rows.Close()

	applied := map[string]bool{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

// migrationNames lists the .sql files in fsys, sorted. Sorting by name is why
// the files are numbered: "00010" sorts after "00009", where "10" would not.
func migrationNames(fsys fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("reading migrations: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, errNoMigrations()
	}
	return names, nil
}

// upSection returns the statements between "-- +goose Up" and "-- +goose
// Down". The markers are goose's so that the same files work unchanged in a
// project already running goose; a file with no markers is used whole.
func upSection(body string) string {
	up := strings.Index(body, "-- +goose Up")
	if up < 0 {
		return body
	}
	rest := body[up+len("-- +goose Up"):]
	if down := strings.Index(rest, "-- +goose Down"); down >= 0 {
		rest = rest[:down]
	}
	return rest
}

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		// Unreachable: the directory is embedded at compile time.
		panic("postgres: embedded schema is missing: " + err.Error())
	}
	return sub
}

func errNoMigrations() error {
	return diagnostic(
		"✗ no migrations found\n\n" +
			"    The filesystem passed to Migrate contains no .sql files.\n\n" +
			"  Pass postgres.Schema for Warren's own tables, or an fs.FS of your\n" +
			"  own numbered files — 00001_name.sql, applied in name order.")
}

func errEmptyMigration(name string) error {
	return diagnostic(fmt.Sprintf(
		"✗ migration %s has no statements\n\n"+
			"    Its \"-- +goose Up\" section is empty.\n\n"+
			"  A file that applies nothing would still be recorded as applied, and\n"+
			"  the real statements would never run. Write the section, or delete\n"+
			"  the file.", name))
}

func errMigrationFailed(name string, cause error) error {
	return diagnostic(fmt.Sprintf(
		"✗ migration %s failed\n\n    %v\n\n"+
			"  It was rolled back and NOT recorded, so fixing the file and running\n"+
			"  again resumes from here — the migrations before it stay applied.",
		name, cause))
}

// readSchemaFile reads one embedded migration. It exists for the test that
// asserts every shipped file has a non-empty Up section.
func readSchemaFile(name string) (string, error) {
	b, err := fs.ReadFile(Schema, name)
	return string(b), err
}
