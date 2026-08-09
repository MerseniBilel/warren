package http_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/MerseniBilel/warren"
	whttp "github.com/MerseniBilel/warren/transport/http"
)

var update = flag.Bool("update", false, "rewrite golden files")

// correlation IDs are unique per request by construction, so a golden file
// pins the shape without pinning the value.
var idPattern = regexp.MustCompile(`"correlation_id":"[^"]*"`)

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("testdata: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("writing golden file: %v", err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden file: %v", err)
	}
	if got != string(want) {
		t.Errorf("does not match golden file %s\ngot:\n%s\nwant:\n%s", path, got, want)
	}
}

// TestErrorBodiesAreGolden pins the wire shape of every response this adapter
// can produce on the error path. The diagnostics ARE the product; untested
// error text rots immediately.
func TestErrorBodiesAreGolden(t *testing.T) {
	t.Parallel()

	base := serve(t, []warren.Module{userModule()})

	var b strings.Builder
	record := func(label, method, path, body string) {
		res, out := do(t, method, base+path, body)
		out = idPattern.ReplaceAllString(out, `"correlation_id":"<id>"`)
		fmt.Fprintf(&b, "── %s\n%s %s → %d", label, method, path, res.StatusCode)
		if allow := res.Header.Get("Allow"); allow != "" {
			fmt.Fprintf(&b, "  Allow: %s", allow)
		}
		if ct := res.Header.Get("Content-Type"); ct != "" {
			fmt.Fprintf(&b, "  Content-Type: %s", ct)
		}
		b.WriteString("\n")
		if out != "" {
			b.WriteString(out)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	record("success", "POST", "/users", `{"email":"bob@example.com"}`)
	record("no content", "DELETE", "/users/u-1", "")
	record("validation", "POST", "/users", `{"name":"Bob"}`)
	record("malformed body", "POST", "/users", `{not json`)
	// Well-formed JSON, wrong type. Recorded because the wire text is the
	// contract: encoding/json's own wording for this names an internal Go
	// struct field, and a 400 a stranger reads must not.
	record("wrong field type", "POST", "/users", `{"email":123}`)
	record("invalid", "POST", "/fail", `{"code":"INVALID"}`)
	record("not found", "POST", "/fail", `{"code":"NOT_FOUND"}`)
	record("conflict", "POST", "/fail", `{"code":"CONFLICT"}`)
	record("unauthenticated", "POST", "/fail", `{"code":"UNAUTHENTICATED"}`)
	record("permission denied", "POST", "/fail", `{"code":"PERMISSION_DENIED"}`)
	record("unavailable", "POST", "/fail", `{"code":"UNAVAILABLE"}`)
	record("internal", "POST", "/fail", `{"code":"leaks a secret"}`)
	record("unknown path", "GET", "/nope", "")
	record("wrong method", "PATCH", "/users/u-1", "")

	assertGolden(t, "responses", b.String())
}

// TestBootDiagnosticsAreGolden pins the boot failures this package produces.
func TestBootDiagnosticsAreGolden(t *testing.T) {
	t.Parallel()

	var b strings.Builder

	m := warren.NewModule("files", warren.Controllers(func() *badRawController { return &badRawController{} }))
	err := warren.New(m, whttp.Server()).Start(context.Background())
	if err == nil {
		t.Fatal("expected a boot failure")
	}
	b.WriteString("── raw route is not an http.Handler\n")
	b.WriteString(err.Error())
	b.WriteString("\n\n")

	c := warren.NewModule("conflict", warren.Controllers(func() *conflictController { return &conflictController{} }))
	err = warren.New(c, whttp.Server()).Start(context.Background())
	if err == nil {
		t.Fatal("expected a boot failure")
	}
	b.WriteString("── conflicting patterns\n")
	b.WriteString(err.Error())
	b.WriteString("\n\n")

	// A registered route with no adapter is core's diagnostic, not this
	// package's, but it names http.Server — so it is pinned where the fix is.
	err = warren.New(userModule()).Start(context.Background())
	if err == nil {
		t.Fatal("expected a boot failure")
	}
	b.WriteString("── routes with no server\n")
	b.WriteString(err.Error())
	b.WriteString("\n")

	assertGolden(t, "boot_diagnostics", b.String())
}
