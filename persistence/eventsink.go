package persistence

import (
	"context"
	"strconv"

	"github.com/MerseniBilel/warren/domain"
	"github.com/MerseniBilel/warren/errors"
)

// EventSink receives the events drained from every aggregate enlisted in a
// committing transaction — inside that transaction, before the commit, in
// enlistment order. A sink that returns an error fails the commit.
type EventSink func(ctx context.Context, events []domain.Event) error

// EventSource is the registration seam for commit sinks. It is deliberately
// NOT part of UnitOfWork: a handler is injected with the port and has no
// business registering a sink, so drivers implement this on their CONCRETE
// unit-of-work type and export it (postgres.UnitOfWork, MemoryUnitOfWork).
//
// Implementing it is not optional for certification. Every driver drains the
// aggregates it enlists, so every driver must have somewhere to put what it
// drained; persistence.RunContract fails a unit of work that does not
// implement EventSource.
type EventSource interface {
	OnCommit(EventSink)
}

// Deliver hands the drained events to the registered sinks and is the ONLY
// way a driver may dispose of them. It refuses when there are events and no
// sink, because dropping them is silent, permanent and undetectable.
//
// A driver calls it between the drain and the commit, on the transaction's
// context, and returns whatever it returns without wrapping.
//
// THE EVENTS ARE ALREADY GONE BY THE TIME THIS REFUSES, and that is worth
// stating plainly rather than leaving to be discovered. PullEvents is
// destructive — that is what makes "drained exactly once" a guarantee the
// enlistment design can rest on — so by the time Deliver can count them they
// no longer exist on the aggregate. The transaction rolls back, so the WRITE
// is not half-done; the events raised by that attempt are lost with it. This
// is accepted because the refusal recurs identically on every subsequent
// request until the wiring is fixed, so nothing is masked, and the aggregate
// instance is discarded with the failed request. The alternative — a
// non-destructive drain — would change domain.Aggregate and cost the
// exactly-once guarantee, which is a far worse trade.
//
// A nil sink is skipped rather than refused. Deliver runs on the commit path,
// and turning a registration mistake into a request failure there is the
// wrong place for it; the registration sites guard nil already.
func Deliver(ctx context.Context, events []domain.Event, sinks []EventSink) error {
	if len(events) > 0 && len(sinks) == 0 {
		// The refusal, and the only new behaviour here. A transaction that
		// raised NOTHING is not a misconfiguration — most transactions raise
		// nothing — so the check is on the pair, not on the sink list alone.
		return ErrNoEventSink(len(events))
	}
	// Sinks still run for an empty drain, which is what the inline loops this
	// replaced did. The spec proposed returning early instead; that was an
	// incidental choice rather than a ruling, and it changed observable
	// behaviour for configurations that were already CORRECT — two contract
	// tests measure "a conflicted transaction announced nothing" by counting
	// sink invocations against commits, and reconstituted aggregates raise
	// nothing, so an early return made a passing driver look like a failing
	// one. A sink handed an empty slice writes nothing; a sink not called at
	// all is a behaviour change nobody asked for.
	for _, sink := range sinks {
		if sink == nil {
			continue
		}
		if err := sink(ctx, events); err != nil {
			return errors.Unavailable("unit of work commit", err)
		}
	}
	return nil
}

// Discard is the sink that says, in writing, that this application's domain
// events go nowhere.
//
// Registering it is how a read-model or projection service answers
// ErrNoEventSink. The point of requiring it is not ceremony: an application
// that discards its events has made a real decision, and this makes that
// decision appear in a diff rather than in the gap between two
// configurations, where nobody reviews it.
//
//	uow.OnCommit(persistence.Discard)
func Discard(context.Context, []domain.Event) error { return nil }

// ErrNoEventSink is the refusal Deliver returns. It is exported for the reason
// ErrNoTransaction and ErrNestedOptions are: every driver returns THIS error,
// and a second copy in another module drifts from it within a month.
//
// It carries INTERNAL rather than UNAVAILABLE deliberately: nothing about the
// next attempt is different, and telling an operator to retry a wiring mistake
// is worse than telling them nothing. Under the §2.6 table that is HTTP 500,
// gRPC Internal, and on a consumer nack-retry-then-DLQ — the message survives,
// which is right for a misconfiguration a deploy will fix.
//
// It is a BARE diagnostic rather than errors.Internal(...) for the same reason
// ErrNoTransaction is: errors.Internal would prefix "unexpected failure:" and
// bury the first line, and an unclassified error already maps to INTERNAL. The
// text is the whole value of this error, so nothing is allowed above it.
func ErrNoEventSink(events int) error {
	noun := "events were"
	if events == 1 {
		noun = "event was"
	}
	return stderrString("✗ this transaction raised domain events and the unit of work has nowhere to put them" + `

    ` + strconv.Itoa(events) + ` ` + noun + ` drained from the aggregates enlisted in this
    transaction, and no commit sink is registered.

  The transaction was ROLLED BACK. A write that commits while its events are
  destroyed is the half-done state this refusal exists to prevent: the row is
  there, the event never happened, and nothing anywhere reports it.

  Answer it one of two ways:

    • PUBLISH them. Turn the outbox on where the unit of work is built, and
      wire a relay to drain it:

          postgres.Module(postgres.DSN(url), postgres.WithOutbox())

      WithOutbox() writes the rows; outbox.NewRelay(store, publisher, …)
      publishes them.

    • DISCARD them, in writing. A read-model or projection service that
      raises events nobody consumes says so:

          uow.OnCommit(persistence.Discard)

      One line, and the decision is in the diff rather than in the gap
      between two configurations.`)
}
