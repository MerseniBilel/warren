package postgres

import (
	"fmt"
	"strings"

	"github.com/MerseniBilel/warren/persistence"
)

// diagnostic carries a rendered multi-line block; the text is the contract,
// covered by golden files like every other Warren diagnostic.
type diagnostic string

func (d diagnostic) Error() string { return string(d) }

// redact removes the password from a DSN so it can appear in a diagnostic, a
// log line, or an error without leaking a credential.
//
// This is the reason every message here takes the DSN through this function
// rather than formatting it directly: a connection failure is the single most
// likely error a service prints, and printing it with the password intact
// puts that password into every log aggregator the service reaches. Both DSN
// forms are handled — the URL form, whose userinfo carries it, and the
// keyword form, where it is `password=…`.
// It does NOT use net/url. url.Parse refuses a malformed DSN — and a
// malformed DSN is precisely when errBadDSN fires, so parsing first and
// redacting second leaks the password on the one path guaranteed to print it.
// That bug shipped for about ten minutes and was caught by this package's own
// golden file; the surgery below is deliberate, and the malformed cases are in
// the table test.
func redact(dsn string) string {
	out := strings.TrimSpace(dsn)
	if out == "" {
		return ""
	}
	// URL form: scheme://user:password@host/db.
	//
	// Find the "@" FIRST, and use the LAST one. Cutting the authority at the
	// first "/" or "?" — the obvious reading — is wrong, because both
	// characters occur inside passwords: `openssl rand -base64` emits "/"
	// routinely, so RDS-generated credentials trip it. Cut there and the "@"
	// falls outside the slice, nothing matches, and the password prints in
	// full on the one path guaranteed to print it. That shipped, and a field
	// test caught it.
	//
	// Using the last "@" can over-redact a path that contains one. That is
	// the right way to be wrong: over-redacting costs legibility, and
	// under-redacting costs a credential.
	if i := strings.Index(out, "://"); i >= 0 {
		rest := out[i+3:]
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			if colon := strings.IndexByte(rest[:at], ':'); colon >= 0 {
				out = out[:i+3] + rest[:colon] + ":xxxxx" + rest[at:]
			}
		}
	}
	// Keyword form, and any `password=` surviving in a query string. Applied
	// to both shapes on purpose: belt and braces on the path that prints
	// credentials.
	return redactKeyword(out)
}

// redactKeyword replaces the value of every password= key, whatever separates
// the pairs: spaces in the keyword DSN form, and "?" or "&" in a URL's query
// string, where libpq also accepts one.
func redactKeyword(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	start := 0
	for start <= len(s) {
		end := len(s)
		if j := strings.IndexAny(s[start:], " &?"); j >= 0 {
			end = start + j
		}
		field := s[start:end]
		if k, _, ok := strings.Cut(field, "="); ok && strings.EqualFold(strings.TrimSpace(k), "password") {
			field = k + "=xxxxx"
		}
		b.WriteString(field)
		if end < len(s) {
			b.WriteByte(s[end])
		}
		start = end + 1
	}
	return b.String()
}

func errNoDSN() error {
	return diagnostic(
		"✗ postgres has no connection string\n\n" +
			"    postgres.Module() was declared without postgres.DSN(...).\n\n" +
			"  Give it one, usually from config:\n\n" +
			"      postgres.Module(postgres.DSN(cfg.DatabaseURL))\n\n" +
			"  Either form works:\n" +
			"      postgres://user:pass@localhost:5432/app?sslmode=disable\n" +
			"      host=localhost user=app dbname=app sslmode=disable")
}

func errBadDSN(dsn string, cause error) error {
	return diagnostic(fmt.Sprintf(
		"✗ postgres connection string is not valid\n\n    %s\n    %v\n\n"+
			"  This is a parse failure, not a connection failure — nothing was\n"+
			"  dialled. Check the scheme, the port, and sslmode.",
		redact(dsn), cause))
}

func errCannotConnect(dsn string, cause error) error {
	return diagnostic(fmt.Sprintf(
		"✗ cannot connect to postgres\n\n    %s\n    %v\n\n"+
			"  The connection string parsed, so this is the database, the network,\n"+
			"  or the credentials. Check that the server is reachable from here and\n"+
			"  that the role exists — using the variable you set the DSN in, since\n"+
			"  the one printed above has its password removed and would fail auth:\n\n"+
			"      psql \"$DATABASE_URL\" -c 'select 1'",
		redact(dsn), cause))
}

func errNotStarted() error {
	return diagnostic(
		"✗ postgres pool used before it was started\n\n" +
			"    The pool is opened by this module's OnStart hook, at boot step 6.\n\n" +
			"  Something reached the database during CONSTRUCTION instead. A\n" +
			"  constructor wires; it does not acquire. Move the query into an\n" +
			"  OnStart hook of your own, which runs after this one.")
}

// errNoTransaction is the refusal that keeps a write from autocommitting
// outside a unit of work, where its events would be silently lost.
//
// The MESSAGE lives in core, and deliberately: every driver must produce the
// same one, and two copies of a diagnostic drift within a month. The
// PREDICATE stays here and is stricter than core's — core asks "is there a
// unit of work", which is what enlistment needs; this asks "and is there a
// Postgres transaction to run SQL on", which is what the statement needs.
func errNoTransaction(op string) error {
	return persistence.ErrNoTransaction(op)
}

func errTableMissing(table string) error {
	return diagnostic(fmt.Sprintf(
		"✗ table %q does not exist\n\n"+
			"    The %s module started, but its table has not been created.\n\n"+
			"  Warren never migrates at boot — under a rolling deploy that races\n"+
			"  every replica and applies DDL the still-serving old ones were not\n"+
			"  written against. Apply the schema as a DEPLOY STEP instead.\n\n"+
			"  Either run Warren's own applier from a small main, or a deploy job:\n\n"+
			"      postgres.Migrate(ctx, os.Getenv(\"DATABASE_URL\"), postgres.Schema)\n\n"+
			"  or point the migration tool you already use at postgres.Schema — the\n"+
			"  files are plain SQL in goose's format, so goose, atlas and dbmate all\n"+
			"  read them unchanged.",
		table, ModuleName))
}
