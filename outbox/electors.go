package outbox

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/MerseniBilel/warren/log"
)

// Electors mints leaderships by name.
//
// A NAME is a leadership. Components with different names lead at the same
// time on the same replica; components sharing a name contend for one — and
// that is the only thing a name is for.
//
// It exists because one Elector is one lock. README used to say a scheduler
// that wants leader-only "injects the same one", and a field test did
// exactly that: an SLA sweeper and the outbox relay on one elector, so
// whichever goroutine woke first took the lock and the other did nothing for
// the life of the process. Four runs of one binary, two each way, both
// reporting /readyz 200.
//
//	el, err := electors.Elector("ticket/sla-sweeper")
//
// Warren's container resolves by TYPE, so this is a second type rather than
// a second binding of the first: there is no keyed resolution to reach for.
type Electors interface {
	// Elector returns the leadership called name, claiming it for this
	// process.
	//
	// CALL IT FROM A CONSTRUCTOR. A claim refused is then a boot failure
	// naming both claimants, and a claim not refused is the guarantee that
	// nothing else in this binary is quietly competing for the same lock.
	// Called lazily on a request path it would still work and would prove
	// nothing.
	Elector(name string) (Elector, error)
}

// StandaloneElectors returns an Electors whose every elector always leads.
// Correct for one instance and for the modular monolith; with several
// replicas, every replica runs the work.
//
// It is also what a test substitutes for the real thing:
//
//	warrentest.Replace[outbox.Electors](outbox.StandaloneElectors())
//
// It claims names exactly as strictly as a real registry does. A double that
// permits what production refuses is a test that passes for a service which
// cannot start.
func StandaloneElectors() Electors {
	return &standaloneElectors{claimed: map[string]bool{}}
}

type standaloneElectors struct {
	mu      sync.Mutex
	claimed map[string]bool
}

func (s *standaloneElectors) Elector(name string) (Elector, error) {
	if name == "" {
		return nil, errUnnamedLeadership()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimed[name] {
		return nil, errLeadershipClaimed(name)
	}
	s.claimed[name] = true
	return &standaloneElector{name: name}, nil
}

type standaloneElector struct {
	name   string
	warned atomic.Bool
}

func (s *standaloneElector) Lead(ctx context.Context, fn func(context.Context) error) error {
	// Standalone() has the relay to warn on its behalf, gated on Durable. A
	// standalone ELECTOR has nobody — nothing else in the process knows it
	// exists — so it says so itself, once, the first time it leads. This
	// project lost half of a field test's events to the silent-standalone
	// case; the new API does not get to repeat it.
	if s.warned.CompareAndSwap(false, true) {
		log.FromContext(ctx).WarnContext(ctx, "leading unconditionally",
			"leadership", s.name,
			"risk", "every replica runs this work, because nothing is electing between them",
			"fix", "wire an elector backed by your database — postgres.WithAdvisoryLock() provides outbox.Electors",
			"safe_if", "this service runs as a single instance")
	}
	return fn(ctx)
}

func errUnnamedLeadership() error {
	return diagnostic(
		"✗ a leadership needs a name\n\n" +
			"    Elector(\"\") was called. An unnamed leadership is one that every\n" +
			"    unnamed claimant shares, so two components would contend for it\n" +
			"    and one of them would silently never run.\n\n" +
			"  Name it for the component that leads:\n\n" +
			"      electors.Elector(\"ticket/sla-sweeper\")")
}

func errLeadershipClaimed(name string) error {
	return diagnostic(fmt.Sprintf(
		"✗ the leadership %q is already claimed\n\n"+
			"    Two components in this process asked for the same leadership,\n"+
			"    and a leadership has one holder — so one of them would never\n"+
			"    run, and which one would be decided by whichever constructor\n"+
			"    the container reached first.\n\n"+
			"  Give them separate names. Leaderships are independent: two names\n"+
			"  lead at the same time, on the same replica.",
		name))
}
