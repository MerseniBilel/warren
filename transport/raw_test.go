package transport_test

import (
	"context"
	"strings"
	"testing"

	werrors "github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/transport"
)

// uploadHandler stands in for what the typed port deliberately does not model:
// a handler that owns its own body reading. Core never looks inside it.
type uploadHandler struct{ name string }

type rawController struct{ h *uploadHandler }

func (c *rawController) Register(r transport.Registrar) {
	transport.Raw(r, transport.ProtocolHTTP, "POST /uploads", c.h)
}

func TestRawRouteReachesTheTable(t *testing.T) {
	t.Parallel()

	h := &uploadHandler{name: "avatars"}
	tbl := build(t, &rawController{h: h})

	raws := tbl.Raw()
	if len(raws) != 1 {
		t.Fatalf("raw routes = %d, want 1", len(raws))
	}
	got := raws[0]
	if got.Protocol != transport.ProtocolHTTP {
		t.Errorf("protocol = %v, want HTTP", got.Protocol)
	}
	if got.Pattern != "POST /uploads" {
		t.Errorf("pattern = %q", got.Pattern)
	}
	if got.Handler != any(h) {
		t.Errorf("handler = %#v, want the registered value verbatim", got.Handler)
	}
	if got.Name != "user.uploadHandler" {
		t.Errorf("name = %q, want <module>.<type>", got.Name)
	}
	// A raw route is not a typed route; nothing decodes for it.
	if len(tbl.HTTP()) != 0 {
		t.Errorf("raw registration leaked into HTTP() = %d routes", len(tbl.HTTP()))
	}
}

// denyAll is a policy that refuses; a raw route must carry it as data, the
// same way a typed route does, because the adapter runs guards before the
// handler and core is what tells it which ones.
type denyAll struct{}

func (denyAll) Authorize(context.Context) error { return werrors.PermissionDenied("upload") }

func TestRawRouteCarriesItsGuardsAndName(t *testing.T) {
	t.Parallel()

	b := transport.NewBuilder()
	r := b.For("user")
	transport.Raw(r, transport.ProtocolHTTP, "POST /uploads", &uploadHandler{},
		transport.Named("user.upload"), transport.Guard(denyAll{}))
	tbl, err := b.Table()
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	got := tbl.Raw()[0]
	if got.Name != "user.upload" {
		t.Errorf("Named did not override: %q", got.Name)
	}
	if len(got.Guards) != 1 {
		t.Fatalf("guards = %d, want 1 — a raw route runs its guards", len(got.Guards))
	}
	if err := got.Guards[0].Authorize(context.Background()); err == nil {
		t.Error("the guard travelled without its behaviour")
	}
}

func TestUnservedCountsRawRoutes(t *testing.T) {
	t.Parallel()

	tbl := build(t, &rawController{h: &uploadHandler{}})

	err := tbl.Unserved()
	if err == nil {
		t.Fatal("a raw HTTP route with no HTTP server must fail the boot")
	}
	if !strings.Contains(err.Error(), "http.Server") {
		t.Errorf("diagnostic must name the fix:\n%s", err)
	}
	tbl.Claim(transport.ProtocolHTTP, "warren/transport/http")
	if err := tbl.Unserved(); err != nil {
		t.Errorf("claimed protocol still unserved: %v", err)
	}
}

func TestRawRefusesEmptyPattern(t *testing.T) {
	t.Parallel()

	b := transport.NewBuilder()
	transport.Raw(b.For("user"), transport.ProtocolHTTP, "", &uploadHandler{})
	if _, err := b.Table(); err == nil {
		t.Fatal("an empty raw pattern must be a boot error")
	}
}

// TestRawNilHandlerIsARegistrationFailure — Raw's nil check was a panic for
// the same unargued reason the typed ones were, in the same function shape,
// three lines from the reg.fail its empty-pattern sibling above already uses.
func TestRawNilHandlerIsARegistrationFailure(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a nil raw handler panicked: %v", r)
		}
	}()
	b := transport.NewBuilder()
	transport.Raw(b.For("user"), transport.ProtocolHTTP, "POST /uploads", nil)
	if _, err := b.Table(); err == nil {
		t.Fatal("a nil raw handler built a table")
	}
}

// TestFillReusesTheBootTable is the seam boot step 5 needs: the Table is
// provided EMPTY in the root scope at step 2, so that an adapter injecting it
// resolves at step 3 — long before any controller has registered — and filled
// in place at step 5.
func TestFillReusesTheBootTable(t *testing.T) {
	t.Parallel()

	provided := &transport.Table{} // what boot hands the container at step 2
	provided.Claim(transport.ProtocolHTTP, "warren/transport/http")

	b := transport.NewBuilder()
	(&userController{}).Register(b.For("user"))
	if err := b.Fill(provided); err != nil {
		t.Fatalf("Fill: %v", err)
	}

	if got := len(provided.HTTP()); got != 2 {
		t.Errorf("HTTP routes = %d, want 2 — Fill must write into the given Table", got)
	}
	// The claim made before Fill survives it: an adapter claims in its
	// constructor, which runs at step 5b, but nothing may depend on that order.
	if err := provided.Unserved(); err == nil || !strings.Contains(err.Error(), "gRPC") {
		t.Errorf("claims made before Fill must survive it; Unserved = %v", err)
	}
}

func TestFillReportsRegistrationErrors(t *testing.T) {
	t.Parallel()

	b := transport.NewBuilder()
	r := b.For("user")
	transport.Raw(r, transport.ProtocolHTTP, "", &uploadHandler{})

	if err := b.Fill(&transport.Table{}); err == nil {
		t.Fatal("Fill must report what Table would have reported")
	}
}
