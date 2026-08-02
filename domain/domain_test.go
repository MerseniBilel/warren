package domain_test

import (
	"testing"
	"time"

	"github.com/MerseniBilel/warren/domain"
)

// orderID is the test identifier type: comparable and a fmt.Stringer, the two
// halves of the domain.ID constraint.
type orderID string

func (id orderID) String() string { return string(id) }

// order is the minimal aggregate the semantics suite runs against.
type order struct {
	domain.AggregateRoot[orderID]
	total int
}

// *AggregateRoot satisfies Root — the seam Repository[T Root[K], K ID] is
// generic over.
var _ domain.Root[orderID] = (*order)(nil)

// placed is the test event.
type placed struct {
	ID orderID
	At time.Time
}

func (p placed) EventName() string     { return "order.placed" }
func (p placed) OccurredAt() time.Time { return p.At }
func (p placed) AggregateID() string   { return p.ID.String() }

func newOrder(id orderID) *order {
	return &order{AggregateRoot: domain.NewAggregateRoot(id)}
}

func TestAggregateSemantics(t *testing.T) {
	t.Parallel()

	t.Run("raise then pull returns the events in the order raised", func(t *testing.T) {
		t.Parallel()
		o := newOrder("o-1")
		first := placed{ID: "o-1", At: time.Unix(1, 0)}
		second := placed{ID: "o-1", At: time.Unix(2, 0)}
		o.Raise(first)
		o.Raise(second)

		events := o.PullEvents()
		if len(events) != 2 || events[0] != domain.Event(first) || events[1] != domain.Event(second) {
			t.Errorf("PullEvents() = %v, want [%v %v] in order", events, first, second)
		}
	})

	t.Run("pull is destructive: a second pull returns nothing", func(t *testing.T) {
		t.Parallel()
		o := newOrder("o-1")
		o.Raise(placed{ID: "o-1", At: time.Unix(1, 0)})
		o.PullEvents()

		if again := o.PullEvents(); len(again) != 0 {
			t.Errorf("second PullEvents() = %v, want none — UnitOfWork relies on this to not publish twice", again)
		}
	})

	t.Run("raising after a pull accumulates fresh events only", func(t *testing.T) {
		t.Parallel()
		o := newOrder("o-1")
		o.Raise(placed{ID: "o-1", At: time.Unix(1, 0)})
		o.PullEvents()
		later := placed{ID: "o-1", At: time.Unix(2, 0)}
		o.Raise(later)

		events := o.PullEvents()
		if len(events) != 1 || events[0] != domain.Event(later) {
			t.Errorf("PullEvents() after a drain = %v, want only the later event", events)
		}
	})

}

// Not parallel, and not a subtest of a parallel parent:
// testing.AllocsPerRun forbids running during a parallel test.
func TestQuietAggregateAllocatesNoEventSlice(t *testing.T) {
	allocs := testing.AllocsPerRun(100, func() {
		o := order{AggregateRoot: domain.NewAggregateRoot[orderID]("o-1")}
		if events := o.PullEvents(); events != nil {
			t.Fatal("PullEvents() on a quiet aggregate returned a non-nil slice")
		}
	})
	if allocs != 0 {
		t.Errorf("quiet aggregate allocated %.0f times per run, want 0", allocs)
	}
}

func TestIdentity(t *testing.T) {
	t.Parallel()

	t.Run("identity is set at construction and read through ID", func(t *testing.T) {
		t.Parallel()
		o := newOrder("o-42")
		if got := o.ID(); got != "o-42" {
			t.Errorf("ID() = %q, want %q", got, "o-42")
		}
	})

	t.Run("equal identifiers mean the same identity", func(t *testing.T) {
		t.Parallel()
		a, b := newOrder("o-1"), newOrder("o-1")
		b.total = 99
		if a.ID() != b.ID() {
			t.Error("two aggregates with the same identifier do not compare equal on identity")
		}
	})

	t.Run("identifiers work as map keys and round-trip through String", func(t *testing.T) {
		t.Parallel()
		seen := map[orderID]bool{"o-1": true}
		if !seen[newOrder("o-1").ID()] {
			t.Error("identifier did not work as a map key")
		}
		if got := newOrder("o-1").ID().String(); got != "o-1" {
			t.Errorf("String() = %q, want %q", got, "o-1")
		}
	})
}

func TestEventConformance(t *testing.T) {
	t.Parallel()

	o := newOrder("o-1")
	at := time.Unix(42, 0)
	o.Raise(placed{ID: o.ID(), At: at})
	e := o.PullEvents()[0]

	name, again := e.EventName(), e.EventName()
	if name == "" || name != again {
		t.Errorf("EventName() = %q then %q, want stable and non-empty", name, again)
	}
	if got := e.AggregateID(); got != o.ID().String() {
		t.Errorf("AggregateID() = %q, want the raising aggregate's %q", got, o.ID().String())
	}
	if first, second := e.OccurredAt(), e.OccurredAt(); !first.Equal(at) || !first.Equal(second) {
		t.Errorf("OccurredAt() = %v then %v, want %v both times", first, second, at)
	}
}

// totalAbove is the sample specification: one rule, evaluated in memory.
// Translating it to a query is a driver's business, not the domain's.
type totalAbove struct{ threshold int }

func (s totalAbove) IsSatisfiedBy(o *order) bool { return o.total > s.threshold }

var _ domain.Specification[*order] = totalAbove{}

func TestSpecificationEvaluatesInMemory(t *testing.T) {
	t.Parallel()

	spec := totalAbove{threshold: 100}
	cases := []struct {
		name  string
		total int
		want  bool
	}{
		{"below the threshold", 50, false},
		{"at the threshold", 100, false},
		{"above the threshold", 150, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := newOrder("o-1")
			o.total = tc.total
			if got := spec.IsSatisfiedBy(o); got != tc.want {
				t.Errorf("IsSatisfiedBy(total=%d) = %v, want %v", tc.total, got, tc.want)
			}
		})
	}
}

func BenchmarkRaiseAndPull(b *testing.B) {
	o := newOrder("o-1")
	e := placed{ID: "o-1", At: time.Unix(1, 0)}
	b.ReportAllocs()
	for b.Loop() {
		o.Raise(e)
		o.PullEvents()
	}
}

func BenchmarkPullEventsEmpty(b *testing.B) {
	o := newOrder("o-1")
	b.ReportAllocs()
	for b.Loop() {
		o.PullEvents()
	}
}
