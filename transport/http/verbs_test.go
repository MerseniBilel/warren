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

// transport.Put and transport.Patch were public API that nothing in the
// repository ever called — no test, no template, no scaffold — and Delete was
// called once. A verb string or a default status is a one-word constant: a
// wrong one compiles, registers, and serves the wrong thing for ever.
//
// warren.md §3.5 states the defaults in one line — "default success 201;
// Delete 204; the rest 200". This is that line, executed.

type verbReq struct {
	ID   string `param:"id"`
	Note string `json:"note"`
}

type verbRes struct {
	Verb string `json:"verb"`
	ID   string `json:"id"`
}

type verbController struct{}

func (c *verbController) put(_ context.Context, q verbReq) (verbRes, error) {
	return verbRes{Verb: "PUT", ID: q.ID}, nil
}

func (c *verbController) patch(_ context.Context, q verbReq) (verbRes, error) {
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

func (c *verbController) post(_ context.Context, q verbReq) (verbRes, error) {
	return verbRes{Verb: "POST", ID: q.ID}, nil
}

func (c *verbController) Register(r transport.Registrar) {
	transport.Get(r, "/things/{id}", app.HandlerFunc[verbReq, verbRes](c.get))
	transport.Post(r, "/things/{id}", app.HandlerFunc[verbReq, verbRes](c.post))
	transport.Put(r, "/things/{id}", app.HandlerFunc[verbReq, verbRes](c.put))
	transport.Patch(r, "/things/{id}", app.HandlerFunc[verbReq, verbRes](c.patch))
	transport.Delete(r, "/things/{id}", app.HandlerFunc[verbReq, verbRes](c.del))
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
