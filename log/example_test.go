package log_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/log"
)

// stdoutLogger returns a logger writing JSON to stdout with the timestamp
// dropped, so that an example's output is comparable.
func stdoutLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}

			return a
		},
	}))
}

// The edge attaches the logger once. Every layer below reads it from the
// context, so nothing takes a logger as a constructor argument.
func ExampleInto() {
	ctx := log.Into(context.Background(), stdoutLogger())

	log.From(ctx).InfoContext(ctx, "order created", "order_id", "abc")
	// Output: {"level":"INFO","msg":"order created","order_id":"abc"}
}

// From falls back to slog's default, so a call site never has to check whether
// a logger was attached.
func ExampleFrom() {
	logger := log.From(context.Background())

	fmt.Println(logger == slog.Default())
	// Output: true
}

// With attaches request-scoped attributes once. Every record logged downstream
// carries them without the call site repeating them.
func ExampleWith() {
	ctx := log.Into(context.Background(), stdoutLogger())
	ctx = log.With(ctx, "request_id", "r-1", "tenant", "acme")

	log.From(ctx).InfoContext(ctx, "order created")
	log.From(ctx).WarnContext(ctx, "stock is low")
	// Output:
	// {"level":"INFO","msg":"order created","request_id":"r-1","tenant":"acme"}
	// {"level":"WARN","msg":"stock is low","request_id":"r-1","tenant":"acme"}
}

// WithAttrs is the typed form. An odd number of arguments to With degrades at
// runtime; this cannot.
func ExampleWithAttrs() {
	ctx := log.Into(context.Background(), stdoutLogger())
	ctx = log.WithAttrs(ctx, slog.String("request_id", "r-1"), slog.Int("attempt", 2))

	log.From(ctx).InfoContext(ctx, "retrying")
	// Output: {"level":"INFO","msg":"retrying","request_id":"r-1","attempt":2}
}

// Err keeps an error's structure instead of flattening it into the message, so
// error.code and error.order_id can be queried.
func ExampleErr() {
	ctx := log.Into(context.Background(), stdoutLogger())

	err := errors.NotFound("no order abc").
		Op("orders.Repository.Get").
		Field("order_id", "abc")

	log.From(ctx).ErrorContext(ctx, "cannot load the order", log.Err(err))
	// Output: {"level":"ERROR","msg":"cannot load the order","error":{"message":"orders.Repository.Get: no order abc","code":"NotFound","ops":["orders.Repository.Get"],"order_id":"abc"}}
}

// Err(nil) emits nothing, so a caller need not branch on whether it has an
// error.
func ExampleErr_nilError() {
	ctx := log.Into(context.Background(), stdoutLogger())

	log.From(ctx).InfoContext(ctx, "finished", log.Err(nil))
	// Output: {"level":"INFO","msg":"finished"}
}
