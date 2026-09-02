package openapi

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/MerseniBilel/warren"
	"github.com/MerseniBilel/warren/lifecycle"
	"github.com/MerseniBilel/warren/transport"
)

// ModuleName is the name of the module Module returns.
const ModuleName = "warren/openapi"

// Module serves the OpenAPI document at /openapi.json.
//
// THE DOCUMENT IS BUILT ONCE, in an OnStart hook after boot step 5 has frozen
// the route table, and the route serves precomputed bytes. Reflection over
// struct tags is a boot activity (invariant 7); a request costs one write of a
// []byte that already exists, whatever the size of the API.
//
// It registers through r.Raw because the document is bytes, not a typed
// handler — and the emitter skips the routes this module owns, so no document
// describes its own delivery mechanism.
func Module(opts ...Option) warren.Module {
	cfg := defaults()
	for _, opt := range opts {
		opt.apply(&cfg)
	}

	return warren.NewModule(ModuleName,
		warren.Providers(
			func(tbl *transport.Table, lc lifecycle.Lifecycle) (*server, error) {
				s := &server{}
				lc.Append(lifecycle.Hook{
					Name: ModuleName,
					OnStart: func(context.Context) error {
						doc, err := Emit(tbl, opts...)
						if err != nil {
							// Only reachable under Strict(): Emit returns the
							// document AND the error otherwise.
							return err
						}
						body, merr := doc.JSON()
						if merr != nil {
							return merr
						}
						s.body = body

						// Every refusal is reported at boot as well as in the
						// document. A team that never opens /openapi.json
						// still learns that part of its API is undescribed.
						for _, r := range doc.Refusals() {
							slog.Warn("openapi could not describe a route",
								"route", r.Route, "type", r.Type, "reason", r.Reason,
								"module", ModuleName)
						}
						return nil
					},
				})
				return s, nil
			},
		),
		warren.Controllers(func(s *server) *controller {
			return &controller{server: s, cfg: cfg}
		}),
	)
}

type server struct{ body []byte }

type controller struct {
	server *server
	cfg    config
}

// Register serves the document. Raw, because the payload is bytes the emitter
// already produced — there is no Req to decode and no Res to encode.
func (c *controller) Register(r *transport.Registrar) {
	opts := make([]transport.RouteOption, 0, len(c.cfg.guards))
	for _, g := range c.cfg.guards {
		opts = append(opts, transport.Guard(g))
	}
	r.Raw(transport.ProtocolHTTP, "GET "+c.cfg.specPath, http.HandlerFunc(c.serve), opts...)
}

func (c *controller) serve(w http.ResponseWriter, _ *http.Request) {
	body := c.server.body
	if body == nil {
		// Unreachable in a booted application: the hook runs at step 6 and
		// routes do not serve before readiness opens. Answering rather than
		// panicking is still the right shape for a handler.
		http.Error(w, "the OpenAPI document is not built yet", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
