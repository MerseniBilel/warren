package errors_test

import (
	stderrors "errors"
	"fmt"
	"net/http"

	"github.com/MerseniBilel/warren/errors"
)

// A repository names the operation that failed and the thing that was missing,
// and wraps the driver's error so nothing is lost.
func ExampleNotFound() {
	err := errors.NotFound("no order %s", "abc").
		Op("orders.Repository.Get").
		Field("order_id", "abc")

	fmt.Println(err)
	// Output: orders.Repository.Get: no order abc
}

// Wrapping keeps the cause reachable by errors.Is, so a caller can still test
// for a driver's sentinel error through a Warren error.
func ExampleError_Wrapping() {
	cause := stderrors.New("dial tcp: connection refused")

	err := errors.Internal("cannot reach the database").
		Op("postgres.Open").
		Wrapping(cause)

	fmt.Println(err)
	fmt.Println(errors.Is(err, cause))
	// Output:
	// postgres.Open: cannot reach the database: dial tcp: connection refused
	// true
}

// A transport maps the semantic code and never reads the message. The
// exhaustive linter fails the build if a new code is added and this switch is
// not updated.
func ExampleCodeOf() {
	err := errors.NotFound("no order abc").Op("orders.Get")

	var status int

	switch errors.CodeOf(err) {
	case errors.CodeNotFound:
		status = http.StatusNotFound
	case errors.CodeConflict:
		status = http.StatusConflict
	case errors.CodeInvalid:
		status = http.StatusBadRequest
	case errors.CodePermissionDenied:
		status = http.StatusForbidden
	case errors.CodeInternal:
		status = http.StatusInternalServerError
	}

	fmt.Println(status)
	// Output: 404
}

// An error from outside Warren is internal, so an unmapped failure becomes a
// 500 rather than a misleading 404.
func ExampleCodeOf_foreignError() {
	fmt.Println(errors.CodeOf(stderrors.New("connection refused")))
	// Output: Internal
}

// Fix records the remedy separately from the message so that a renderer can
// present it distinctly.
func ExampleError_Fix() {
	err := errors.NotFound("no provider for *sql.DB").
		Op("di.Resolve").
		Fix("add warren.Provide(NewDB) to internal/platform/module.go")

	fmt.Println(errors.Fix(err))
	// Output: add warren.Provide(NewDB) to internal/platform/module.go
}

// Detail is the rendering for a terminal: the message, the fields aligned into
// a column, and the fix last.
func ExampleDetail() {
	err := errors.NotFound("no provider for *sql.DB").
		Op("di.Resolve").
		Op("warren.Run").
		Field("requested by", "internal/modules/orders/module.go:14").
		Field("chain", "*OrdersHandler → *OrderRepository → *sql.DB").
		Fix("add warren.Provide(NewDB) to internal/platform/module.go")

	fmt.Println(errors.Detail(err))
	// Output:
	// warren.Run: di.Resolve: no provider for *sql.DB
	//
	//   requested by  internal/modules/orders/module.go:14
	//   chain         *OrdersHandler → *OrderRepository → *sql.DB
	//
	//   fix: add warren.Provide(NewDB) to internal/platform/module.go
}

// Ops read outermost first, which is the order a reader wants: the operation
// they invoked, then the one that actually failed.
func ExampleOps() {
	err := errors.Internal("no provider for *sql.DB").
		Op("di.Resolve").
		Op("di.Build").
		Op("warren.Run")

	fmt.Println(errors.Ops(err))
	// Output: [warren.Run di.Build di.Resolve]
}

// Fields survive being wrapped, so a log line at the top of the stack still
// carries the context attached at the bottom.
func ExampleFields() {
	err := errors.Internal("boot failed").Field("phase", "start").
		Wrapping(errors.Internal("refused").Field("port", 5432))

	for _, f := range errors.Fields(err) {
		fmt.Printf("%s=%v\n", f.Key, f.Value)
	}
	// Output:
	// phase=start
	// port=5432
}
