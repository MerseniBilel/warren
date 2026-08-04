package http

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MerseniBilel/warren/app"
	"github.com/MerseniBilel/warren/health"
	"github.com/MerseniBilel/warren/lifecycle"
	"github.com/MerseniBilel/warren/transport"
)

// The allocation budget is a committed number, not an adjective. Invariant 7
// is a performance claim, and a claim without a number is a slogan.

type allocReq struct {
	Email string `json:"email"`
	ID    string `param:"id"`
	Trace string `query:"trace"`
}

type allocRes struct {
	ID string `json:"id"`
}

type allocController struct{}

func (allocController) handle(_ context.Context, r allocReq) (allocRes, error) {
	return allocRes{ID: r.ID}, nil
}

func (c allocController) Register(r transport.Registrar) {
	transport.Post(r, "/users/{id}", app.HandlerFunc[allocReq, allocRes](c.handle))
}

// nullWriter is a ResponseWriter that allocates nothing, so what the
// benchmark reports is the adapter's cost and not the recorder's.
type nullWriter struct{ h http.Header }

func (w *nullWriter) Header() http.Header         { return w.h }
func (w *nullWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *nullWriter) WriteHeader(int)             {}

// reusableBody lets one request be served many times without allocating a
// fresh body each run.
type reusableBody struct{ *bytes.Reader }

func (reusableBody) Close() error { return nil }

func newTestServer(t testing.TB) *server { return newTestServerWith(t) }

// newTestServerWith is newTestServer with options — how the codec tests get a
// server that is identical except for the one thing under test.
func newTestServerWith(t testing.TB, opts ...Option) *server {
	t.Helper()
	b := transport.NewBuilder()
	allocController{}.Register(b.For("bench"))
	tbl, err := b.Table()
	if err != nil {
		t.Fatalf("table: %v", err)
	}
	lc := lifecycle.New()
	cfg := defaults()
	for _, o := range opts {
		o.apply(&cfg)
	}
	s, err := newServer(cfg, tbl, lc, health.New(func() bool { return true }))
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	return s
}

func requestFor(payload []byte) (*http.Request, *bytes.Reader) {
	rd := bytes.NewReader(payload)
	req, _ := http.NewRequest("POST", "/users/u-1?trace=abc", nil)
	req.Body = reusableBody{rd}
	req.ContentLength = int64(len(payload))
	return req, rd
}

func TestAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector adds allocations; the budget is measured without it")
	}
	s := newTestServer(t)
	payload := []byte(`{"email":"bob@example.com"}`)
	req, rd := requestFor(payload)
	w := &nullWriter{h: http.Header{}}

	got := int(testing.AllocsPerRun(200, func() {
		rd.Reset(payload)
		req.Body = reusableBody{rd}
		s.mux.ServeHTTP(w, req)
	}))

	// The committed budget, and where every allocation goes — measured on
	// go1.26.3, darwin/arm64:
	//
	//	 2  net/http.ServeMux dispatch with one path wildcard
	//	 6  edge ring: the ID string, the response header slice, the
	//	    correlation context value, and the http.Request clone
	//	    r.WithContext makes so user middleware sees the ID
	//	10  the typed path, of which ~7 are encoding/json's decoder
	//	--
	//	18
	//
	// Raise it only with a measurement and a reason in the commit message. A
	// silently drifting number is the thing this test exists to prevent —
	// and note the reference point BenchmarkHandlerDirect prints: the same
	// handler called without a transport allocates 0.
	const budget = 18
	if got > budget {
		t.Errorf("POST with a JSON body and a path and query parameter allocates %d, budget %d", got, budget)
	}
	t.Logf("allocations per request: %d (budget %d)", got, budget)
}

func BenchmarkRequest(b *testing.B) {
	s := newTestServer(b)
	payload := []byte(`{"email":"bob@example.com"}`)
	req, rd := requestFor(payload)
	w := &nullWriter{h: http.Header{}}

	b.ReportAllocs()
	for b.Loop() {
		rd.Reset(payload)
		req.Body = reusableBody{rd}
		s.mux.ServeHTTP(w, req)
	}
}

// BenchmarkHandlerDirect is the reference point: the same handler called
// without a transport at all. The gap between the two is what HTTP costs.
func BenchmarkHandlerDirect(b *testing.B) {
	h := app.HandlerFunc[allocReq, allocRes](allocController{}.handle)
	ctx := context.Background()
	req := allocReq{Email: "bob@example.com", ID: "u-1", Trace: "abc"}

	b.ReportAllocs()
	for b.Loop() {
		if _, err := h.Handle(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
}

// TestStrictCodecAllocations prices the opt-in, so the next person to
// consider flipping the default reads the cost here rather than
// rediscovering it.
//
// json.Decoder has no Reset in encoding/json v1 — go doc encoding/json
// Decoder lists Buffered, Decode, DisallowUnknownFields, InputOffset, More,
// Token, UseNumber and nothing else — so the reader and the decoder are both
// per-request and cannot be pooled the way bodyPool pools the read buffer.
// That is a floor, not an implementation detail.
//
// TestAllocations above stays at 18 and is asserted against the DEFAULT
// codec. That it did not move is itself the assertion that this change was
// additive.
func TestStrictCodecAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector adds allocations; the budget is measured without it")
	}
	s := newTestServerWith(t, Codec(transport.StrictJSON()))
	payload := []byte(`{"email":"bob@example.com"}`)
	req, rd := requestFor(payload)
	w := &nullWriter{h: http.Header{}}

	got := int(testing.AllocsPerRun(200, func() {
		rd.Reset(payload)
		req.Body = reusableBody{rd}
		s.mux.ServeHTTP(w, req)
	}))

	// 18 for the default path, +3 for the unpoolable reader and decoder.
	const budget = 21
	if got > budget {
		t.Errorf("strict codec allocates %d per request, budget %d", got, budget)
	}
	t.Logf("strict codec allocations per request: %d (budget %d)", got, budget)
}

// TestStrictCodecRejectsUnknownFieldsThroughTheServer — the option is wired
// all the way to the decoder, and the default server still accepts the same
// body. One test, both directions: without the second half, an option that
// silently did nothing would pass.
func TestStrictCodecRejectsUnknownFieldsThroughTheServer(t *testing.T) {
	// Misspelled on purpose: an undeclared member is the subject of the test.
	//
	//nolint:misspell // "nmae" is the defect under test, not a typo in prose
	const typo = `"nmae":"typo"`
	body := []byte(`{"email":"bob@example.com",` + typo + `}`)

	strict := newTestServerWith(t, Codec(transport.StrictJSON()))
	rec := httptest.NewRecorder()
	strict.mux.ServeHTTP(rec, postWith(body))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("strict server returned %d for an unknown member, want 400:\n%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "nmae") { //nolint:misspell // the offending field, quoted back
		t.Errorf("the 400 body does not name the offending field:\n%s", rec.Body)
	}

	lenient := newTestServer(t)
	rec = httptest.NewRecorder()
	lenient.mux.ServeHTTP(rec, postWith(body))
	if rec.Code != http.StatusCreated {
		t.Errorf("the default server returned %d for an unknown member, want 201:\n%s", rec.Code, rec.Body)
	}
}

// TestStrictCodecRejectsTrailingDataThroughTheServer — the More() check,
// end to end. json.Decoder alone would have answered 201 here.
func TestStrictCodecRejectsTrailingDataThroughTheServer(t *testing.T) {
	s := newTestServerWith(t, Codec(transport.StrictJSON()))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, postWith([]byte(`{"email":"a@example.com"} {"email":"b@example.com"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("strict server returned %d for a second document, want 400:\n%s", rec.Code, rec.Body)
	}
}

func postWith(payload []byte) *http.Request {
	req := httptest.NewRequest("POST", "/users/u-1?trace=abc", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	return req
}
