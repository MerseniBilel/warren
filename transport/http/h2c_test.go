package http_test

import (
	"context"
	"net"
	"net/http"
	"testing"

	"github.com/MerseniBilel/warren"
	whttp "github.com/MerseniBilel/warren/transport/http"
)

// H2C is what a service mesh speaks to a sidecar: HTTP/2 with no TLS. The
// option had no test, and an option that silently does nothing is visible
// only in production, as a mesh quietly falling back or failing.
//
// Both directions are asserted. Without the pairing, a test that H2C works
// cannot tell the option from a server that would have accepted h2c anyway.

// h2cClient dials plaintext HTTP/2 with prior knowledge — no upgrade dance,
// which is what a sidecar does.
func h2cClient(on bool) *http.Client {
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(on)
	p.SetHTTP1(!on)
	return &http.Client{Transport: &http.Transport{Protocols: p}}
}

func serveWith(t *testing.T, opts ...whttp.Option) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	opts = append([]whttp.Option{whttp.Listener(ln), whttp.DrainDelay(0)}, opts...)
	a := warren.New(userModule(), whttp.Server(opts...))
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(func() {
		if err := a.Stop(context.Background()); err != nil {
			t.Errorf("stop: %v", err)
		}
	})
	return "http://" + ln.Addr().String()
}

func TestH2CServesUnencryptedHTTP2(t *testing.T) {
	t.Parallel()
	base := serveWith(t, whttp.H2C())

	client := h2cClient(true)
	// Measured: an idle HTTP/2 connection costs ~1.06s at Stop — Go's
	// graceful shutdown sends GOAWAY and waits out a ping — against 1.4ms
	// with the connection closed first and 0.1ms on HTTP/1.1. It is inside
	// the drain budget and not this test's subject, so it is not paid here.
	t.Cleanup(client.CloseIdleConnections)

	res, err := client.Get(base + "/users/u-1")
	if err != nil {
		t.Fatalf("h2c request: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != 200 {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
	if res.ProtoMajor != 2 {
		t.Errorf("served HTTP/%d.%d, want HTTP/2", res.ProtoMajor, res.ProtoMinor)
	}
}

func TestH2CLeavesHTTP1Working(t *testing.T) {
	t.Parallel()
	// A mesh fronts a server that browsers and curl still reach directly.
	base := serveWith(t, whttp.H2C())

	res, err := h2cClient(false).Get(base + "/users/u-1")
	if err != nil {
		t.Fatalf("http/1.1 request: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != 200 {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
	if res.ProtoMajor != 1 {
		t.Errorf("served HTTP/%d.%d, want HTTP/1.1", res.ProtoMajor, res.ProtoMinor)
	}
}

func TestWithoutH2CUnencryptedHTTP2IsRefused(t *testing.T) {
	t.Parallel()
	// The control. If this passed too, the option would be doing nothing and
	// the test above would be certifying net/http's default.
	base := serveWith(t)

	res, err := h2cClient(true).Get(base + "/users/u-1")
	if err == nil {
		defer func() { _ = res.Body.Close() }()
		t.Fatalf("h2c was served without the option: HTTP/%d.%d %d",
			res.ProtoMajor, res.ProtoMinor, res.StatusCode)
	}
}
