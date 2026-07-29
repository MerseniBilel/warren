// Package orders holds the types di's own tests wire together.
//
// It exists so that a golden error message reads like real output: reflect
// renders a named type with its package qualifier, so a chain through a type
// declared in a _test.go file would read "*di_test.Handler" and teach a reader
// nothing. The v0.1 exit criterion is the text of a message, so the text is
// produced from types that look like a service's own.
package orders

import (
	"context"
	"database/sql"
)

// Repository is an infrastructure type: it depends on a driver handle, which is
// the dependency a service most often forgets to provide.
type Repository struct {
	DB *sql.DB
}

// NewRepository returns a Repository backed by db.
func NewRepository(db *sql.DB) *Repository { return &Repository{DB: db} }

// Handler is an application type, one layer above Repository.
type Handler struct {
	Repo *Repository
}

// NewHandler returns a Handler that reads through repo.
func NewHandler(repo *Repository) *Handler { return &Handler{Repo: repo} }

// Port is a repository port, deliberately not satisfied by [Repository]: it is
// what a constructor registered against the wrong interface fails to provide.
type Port interface {
	Save(ctx context.Context, id string) error
}
