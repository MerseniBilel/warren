package http_test

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/MerseniBilel/warren"
	"github.com/MerseniBilel/warren/app"
	"github.com/MerseniBilel/warren/transport"
)

// r.Put and r.Patch were public API that nothing in the
// repository ever called — no test, no template, no scaffold — and Delete was
// called once. A verb string or a default status is a one-word constant: a
// wrong one compiles, registers, and serves the wrong thing for ever.
//
// warren.md §3.5 states the defaults in one line — "default success 201;
// Delete 204; the rest 200". This is that line, executed.

// verbReq is the BODYLESS shape: GET and DELETE carry no request body, so a
// `json:` field on one of them can never be populated and transport refuses
// the route. This fixture used to carry one for all five verbs — a body field
// on a GET — which is finding 1's third variant and is now a boot failure.
type verbReq struct {
	ID string `param:"id"`
}

// verbBodyReq is the same request for the three verbs that do carry a body.
type verbBodyReq struct {
	ID   string `param:"id"`
	Note string `json:"note"`
}

type verbRes struct {
	Verb string `json:"verb"`
	ID   string `json:"id"`
}

type verbController struct{}

func (c *verbController) put(_ context.Context, q verbBodyReq) (verbRes, error) {
	return verbRes{Verb: "PUT", ID: q.ID}, nil
}

func (c *verbController) patch(_ context.Context, q verbBodyReq) (verbRes, error) {
	return verbRes{Verb: "PATCH", ID: q.ID}, nil
}

func (c *verbController) del(_ context.Context, q verbReq) (verbRes, error) {
	// A body deliberately: 204 must drop it, and a handler cannot know which
	// status its route was registered with.
	return verbRes{Verb: "DELETE", ID: q.ID}, nil
}

func (c *verbController) get(_ context.Context, q verbReq) (verbRes, error) {
	return verbRes{Verb: "GET", ID: q.ID}, nil
}

func (c *verbController) post(_ context.Context, q verbBodyReq) (verbRes, error) {
	return verbRes{Verb: "POST", ID: q.ID}, nil
}

func (c *verbController) Register(r *transport.Registrar) {
	r.Get("/things/{id}", app.HandlerFunc[verbReq, verbRes](c.get))
	r.Post("/things/{id}", app.HandlerFunc[verbBodyReq, verbRes](c.post))
	r.Put("/things/{id}", app.HandlerFunc[verbBodyReq, verbRes](c.put))
	r.Patch("/things/{id}", app.HandlerFunc[verbBodyReq, verbRes](c.patch))
	r.Delete("/things/{id}", app.HandlerFunc[verbReq, verbRes](c.del))
}

func verbModule() warren.Module {
	return warren.NewModule("verbs",
		warren.Controllers(func() *verbController { return &verbController{} }),
	)
}

func TestEveryVerbHelperServesItsOwnMethod(t *testing.T) {
	t.Parallel()
	base := serve(t, []warren.Module{verbModule()})

	for _, tc := range []struct {
		method string
		status int // the helper's documented default
		body   bool
	}{
		{"GET", 200, true},
		{"POST", 201, true},
		{"PUT", 200, true},
		{"PATCH", 200, true},
		{"DELETE", 204, false},
	} {
		t.Run(tc.method, func(t *testing.T) {
			res, body := do(t, tc.method, base+"/things/t-1", `{"note":"n"}`)

			if res.StatusCode != tc.status {
				t.Errorf("status = %d, want %d — %s's documented default", res.StatusCode, tc.status, tc.method)
			}
			if !tc.body {
				// 204 forbids a body. Writing one anyway makes Go's own
				// server log and drop it, so the route looks fine and the
				// payload silently never arrives.
				if body != "" {
					t.Errorf("204 carried a body: %q", body)
				}
				if ct := res.Header.Get("Content-Type"); ct != "" {
					t.Errorf("204 carried Content-Type: %q", ct)
				}
				return
			}
			// Each verb must reach ITS OWN handler: five routes share one
			// pattern, and a helper registering the wrong verb string would
			// serve a neighbour's handler with no error anywhere.
			if !strings.Contains(body, `"verb":"`+tc.method+`"`) {
				t.Errorf("%s reached the wrong handler: %s", tc.method, body)
			}
			if !strings.Contains(body, `"id":"t-1"`) {
				t.Errorf("%s did not bind the path parameter: %s", tc.method, body)
			}
		})
	}
}

func TestAnUnregisteredVerbIs405WithEveryRegisteredOneInAllow(t *testing.T) {
	t.Parallel()
	base := serve(t, []warren.Module{verbModule()})

	res, _ := do(t, "OPTIONS", base+"/things/t-1", "")
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", res.StatusCode)
	}
	allow := strings.Split(res.Header.Get("Allow"), ", ")
	sort.Strings(allow)
	// HEAD rides along with GET — net/http serves it from the GET route, so
	// omitting it from Allow would advertise less than the server answers.
	want := []string{"DELETE", "GET", "HEAD", "PATCH", "POST", "PUT"}
	if strings.Join(allow, ",") != strings.Join(want, ",") {
		t.Errorf("Allow = %v, want %v", allow, want)
	}
}

// --- literal beside wildcard: field test #16, finding 1 --------------------
//
// `/users/me`, `/orders/pending`, `/skus/search` — a literal segment beside a
// sibling wildcard — could not be registered until 2026-09-02, and the boot
// diagnostic blamed net/http for it. stdlib accepts every pair below; Warren
// refused them because it registered a METHOD-LESS pattern per path on top of
// each "METHOD /pattern", and a method-less literal against a method-specific
// wildcard is one of the few combinations ServeMux really does reject.
//
// `warren g command --method get --route '/skus/search'` generates this shape,
// so the framework's own tool produced projects the framework would not boot.

type skuReq struct {
	SKU string `param:"sku"`
}

type altSKUReq struct {
	ID string `param:"id"`
}

type searchReq struct {
	Q string `query:"q"`
}

type skuRes struct {
	Route string `json:"route"`
}

type literalController struct{}

func (literalController) Register(r *transport.Registrar) {
	// The three shapes, in one controller, exactly as a real service writes
	// them: a wildcard, a LITERAL sibling of that wildcard, and the same path
	// under a second method with a DIFFERENTLY-NAMED wildcard.
	r.Get("/skus/{sku}", app.HandlerFunc[skuReq, skuRes](
		func(_ context.Context, q skuReq) (skuRes, error) { return skuRes{Route: "wildcard:" + q.SKU}, nil }))
	r.Get("/skus/search", app.HandlerFunc[searchReq, skuRes](
		func(_ context.Context, q searchReq) (skuRes, error) { return skuRes{Route: "search:" + q.Q}, nil }))
	r.Delete("/skus/{id}", app.HandlerFunc[altSKUReq, skuRes](
		func(_ context.Context, q altSKUReq) (skuRes, error) { return skuRes{Route: "delete:" + q.ID}, nil }))
}

func literalModule() warren.Module {
	return warren.NewModule("literal",
		warren.Controllers(func() *literalController { return &literalController{} }),
	)
}

// TestALiteralSegmentServesBesideItsSiblingWildcard — it must BOOT, and the
// literal must win the request they share, which is stdlib's own precedence
// rule and the reason the pair is legal.
func TestALiteralSegmentServesBesideItsSiblingWildcard(t *testing.T) {
	t.Parallel()
	base := serve(t, []warren.Module{literalModule()})

	for _, tc := range []struct {
		name, method, path, want string
		status                   int
	}{
		{"the literal wins the path it shares", "GET", "/skus/search?q=widget", `"route":"search:widget"`, 200},
		{"the wildcard still serves everything else", "GET", "/skus/abc", `"route":"wildcard:abc"`, 200},
		// Delete's documented default is 204, which carries no body — the
		// point here is that the route REGISTERED and dispatched at all.
		{"a second method with a differently-named wildcard", "DELETE", "/skus/abc", "", 204},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, body := do(t, tc.method, base+tc.path, "")
			if res.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d — body %s", res.StatusCode, tc.status, body)
			}
			if tc.want != "" && !strings.Contains(body, tc.want) {
				t.Errorf("body = %s, want %s", body, tc.want)
			}
		})
	}
}

// TestAllowIsDerivedFromTheRoutesThatExist — the 405 behaviour the field test
// praised, preserved after the method-less shims were deleted. It never came
// from stdlib: the shim computed Allow itself, and now the catch-all does, by
// asking the mux which methods this path would have matched.
//
// HEAD is the detail worth pinning. ServeMux serves HEAD from a GET pattern,
// so HEAD belongs in Allow even though no route declares it — the old
// hand-built table had to remember that, and asking the mux gets it for free.
func TestAllowIsDerivedFromTheRoutesThatExist(t *testing.T) {
	t.Parallel()
	base := serve(t, []warren.Module{literalModule()})

	for _, tc := range []struct {
		name, method, path string
		status             int
		allow              string
	}{
		{"wrong method on a wildcard path", "POST", "/skus/abc", 405, "DELETE, GET, HEAD"},
		// /skus/search is served by GET, and DELETE /skus/{id} also matches
		// it — a DELETE of that path really would reach the wildcard handler,
		// so reporting DELETE is the truth about this server.
		{"wrong method on the literal path", "POST", "/skus/search", 405, "DELETE, GET, HEAD"},
		{"a path that does not exist at all", "POST", "/nope", 404, ""},
		{"a path that does not exist, right method", "GET", "/nope", 404, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, body := do(t, tc.method, base+tc.path, "")
			if res.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d — body %s", res.StatusCode, tc.status, body)
			}
			if got := res.Header.Get("Allow"); got != tc.allow {
				t.Errorf("Allow = %q, want %q", got, tc.allow)
			}
			wantCode := `"code":"METHOD_NOT_ALLOWED"`
			if tc.status == 404 {
				wantCode = `"code":"NOT_FOUND"`
			}
			if !strings.Contains(body, wantCode) {
				t.Errorf("body = %s, want %s", body, wantCode)
			}
		})
	}
}

// TestTheHealthProbesSurviveTheRegistrationChange — GET /healthz and
// GET /readyz are registered method-specifically and bypass the edge ring.
// Deleting the method-less shims must not touch them.
func TestTheHealthProbesSurviveTheRegistrationChange(t *testing.T) {
	t.Parallel()
	base := serve(t, []warren.Module{literalModule()})

	for _, path := range []string{"/healthz", "/readyz"} {
		res, body := do(t, "GET", base+path, "")
		if res.StatusCode != 200 {
			t.Errorf("GET %s = %d, want 200 — body %s", path, res.StatusCode, body)
		}
	}
	// A wrong method on a probe is now a 405 rather than a 404, because the
	// catch-all asks the mux and the mux knows GET /healthz exists. That is a
	// deliberate consequence of deriving Allow from the routes: the path does
	// exist, and answering NOT_FOUND for it was the lie the 405 envelope was
	// introduced to stop telling elsewhere.
	res, _ := do(t, "POST", base+"/healthz", "")
	if res.StatusCode != 405 {
		t.Errorf("POST /healthz = %d, want 405", res.StatusCode)
	}
	if got := res.Header.Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q, want %q", got, "GET, HEAD")
	}
}
