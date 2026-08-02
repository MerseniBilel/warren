package http

import (
	"net/http"
	"net/url"
	"strings"
)

// params is the transport.Params the route closure binds `param:` and
// `query:` tags from, using setters core computed at boot.
//
// It is a struct with one pointer field, which Go represents directly inside
// an interface — so putting it on the context costs no allocation of its own.
// Make it two fields and that stops being true.
type params struct{ r *http.Request }

// Path returns a path wildcard — "{id}" in "/users/{id}". net/http resolves
// it from the matched pattern at zero allocations.
func (p params) Path(name string) (string, bool) {
	v := p.r.PathValue(name)
	return v, v != ""
}

// Query returns a query parameter by scanning RawQuery directly.
//
// r.URL.Query() costs 4 allocations and 432 B: it parses and maps EVERY
// parameter to answer one question. A route binds a handful of named
// parameters, so scanning for each is both fewer allocations and less work.
// Do not "simplify" this to Query().
func (p params) Query(name string) (string, bool) {
	q := p.r.URL.RawQuery
	for q != "" {
		var pair string
		if i := strings.IndexByte(q, '&'); i >= 0 {
			pair, q = q[:i], q[i+1:]
		} else {
			pair, q = q, ""
		}
		if pair == "" {
			continue
		}
		key, value, _ := strings.Cut(pair, "=")
		// An escaped key is rare enough to be worth unescaping only when the
		// cheap comparison could still match.
		if key != name {
			if !strings.ContainsAny(key, "%+") {
				continue
			}
			decoded, err := url.QueryUnescape(key)
			if err != nil || decoded != name {
				continue
			}
		}
		// QueryUnescape returns the input unchanged, without allocating, when
		// there is nothing to unescape.
		decoded, err := url.QueryUnescape(value)
		if err != nil {
			return value, true
		}
		return decoded, true
	}
	return "", false
}
