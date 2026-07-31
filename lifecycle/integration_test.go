package lifecycle_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/MerseniBilel/warren/di"
	"github.com/MerseniBilel/warren/lifecycle"
	"github.com/MerseniBilel/warren/log"
)

// The three components a service wires, with the dependencies a real one has:
// the consumer needs the pool, and the server needs the consumer.
type (
	pool     struct{}
	consumer struct{ pool *pool }
	server   struct{ consumer *consumer }
)

func newPool(h *lifecycle.Hooks, rec *recorder) *pool {
	h.Append(rec.hook(poolHook))

	return &pool{}
}

func newConsumer(h *lifecycle.Hooks, rec *recorder, p *pool) *consumer {
	h.Append(rec.hook(consumerHook))

	return &consumer{pool: p}
}

func newServer(h *lifecycle.Hooks, rec *recorder, c *consumer) *server {
	h.Append(rec.hook(serverHook))

	return &server{consumer: c}
}

// TestHooksArriveInDependencyOrder is the claim SPEC.md §5.1 rests on, tested
// rather than asserted: this package records the order it was given and never
// computes one, which is only correct because di.Build runs constructors in
// dependency order (di/SPEC.md §7).
//
// The providers are registered in the wrong order deliberately. If hook order
// followed registration order the assertions below would fail, and the whole
// design — reverse stop as the drain guarantee — would be unsound.
func TestHooksArriveInDependencyOrder(t *testing.T) {
	t.Parallel()

	var rec recorder

	hooks := lifecycle.New()

	c := di.New()
	di.Supply(c, hooks)
	di.Supply(c, &rec)

	// Registration order: server, pool, consumer. Dependency order: pool,
	// consumer, server.
	di.Provide[*server](c, newServer)
	di.Provide[*pool](c, newPool)
	di.Provide[*consumer](c, newConsumer)

	ctx := context.Background()

	err := c.Build(ctx)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	want := []string{poolHook, consumerHook, serverHook}

	got := hooks.Names()
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Names()[%d] = %q, want %q — registration order leaked through", i, got[i], want[i])
		}
	}

	err = hooks.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	err = hooks.Stop(ctx)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}

	const wantSeq = "start:postgres.pool start:kafka.consumer start:http.server " +
		"stop:http.server stop:kafka.consumer stop:postgres.pool"

	if seq := rec.seq(); seq != wantSeq {
		t.Errorf("sequence:\n got %s\nwant %s", seq, wantSeq)
	}
}

// TestEachHookIsLogged covers SPEC.md §5.7: an operator watching a slow boot
// needs to know which component is slow, and the logger is read from the
// context rather than held, so New keeps its empty signature.
func TestEachHookIsLogged(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := log.Into(context.Background(), logger)

	h := lifecycle.New()
	h.Append(lifecycle.Close(poolHook, func() {}))

	err := h.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	err = h.Stop(ctx)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}

	logged := buf.String()

	for _, want := range []string{
		`hook=postgres.pool`,
		`stopping hook`,
		`took=`,
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("the log is missing %q:\n%s", want, logged)
		}
	}
}
