//go:build integration

package postgres_test

import (
	"context"
	"testing"

	"github.com/MerseniBilel/warren"
	"github.com/MerseniBilel/warren/persistence/postgres"
)

// TestMeasureBaselineNoOutbox is the MEASUREMENT of FT14-2 against a real
// server. It is temporary and is deleted once the refusal ships.
func TestMeasureBaselineNoOutbox(t *testing.T) {
	url := isolated(t)

	var (
		db  postgres.DB
		uow *postgres.UnitOfWork
	)
	// NO WithOutbox() — the supported configuration that loses events.
	pgModule := postgres.Module(postgres.DSN(url))
	probe := warren.NewModule("probe",
		warren.Imports(pgModule),
		warren.Providers(func(d postgres.DB, u *postgres.UnitOfWork) *captured {
			db, uow = d, u
			return &captured{}
		}),
		warren.Eager[*captured](),
	)
	a := warren.New(pgModule, probe)
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })

	ctx := context.Background()
	if _, err := db(ctx).Exec(ctx,
		`CREATE TABLE IF NOT EXISTS users (id TEXT PRIMARY KEY, email TEXT NOT NULL)`); err != nil {
		t.Fatalf("create users: %v", err)
	}

	repo := userRepo{db: db}
	u := newUser("m-1", "bob@example.com")
	err := uow.Do(ctx, func(ctx context.Context) error { return repo.Save(ctx, u) })
	t.Logf("MEASUREMENT Do error            = %v", err)

	var rows int
	if qerr := db(ctx).QueryRow(ctx, `SELECT count(*) FROM users WHERE id = 'm-1'`).Scan(&rows); qerr != nil {
		t.Fatalf("count users: %v", qerr)
	}
	t.Logf("MEASUREMENT users rows          = %d", rows)

	var obrows int
	if qerr := db(ctx).QueryRow(ctx, `SELECT count(*) FROM warren_outbox`).Scan(&obrows); qerr != nil {
		t.Fatalf("count warren_outbox: %v", qerr)
	}
	t.Logf("MEASUREMENT warren_outbox rows  = %d", obrows)

	t.Logf("MEASUREMENT PullEvents() after  = %d", len(u.PullEvents()))
}
