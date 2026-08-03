package inbox_test

import (
	"testing"

	"github.com/MerseniBilel/warren/inbox"
	"github.com/MerseniBilel/warren/inbox/inboxtest"
)

// TestContract runs the exported suite every inbox store must pass. The
// durable stores — Postgres joining the unit of work, Redis with native
// TTLs — run this same function, and the call is deliberately identical to
// the one a driver writes.
func TestContract(t *testing.T) {
	t.Parallel()
	inboxtest.Run(t, func(*testing.T) inbox.Store {
		return inbox.NewMemoryStore()
	})
}
