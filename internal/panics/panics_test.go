package panics_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/MerseniBilel/warren/internal/panics"
)

// panickingHelper is a package-level function so it has a name and a file:line
// to report. Frames[0] must be this, on every path below.
func panickingHelper() { panic("boom") }

func TestDoReturnsNilWhenNothingPanics(t *testing.T) {
	t.Parallel()

	ran := false
	if c := panics.Do(func() { ran = true }); c != nil {
		t.Fatalf("Do reported a panic that did not happen: %v", c.Value)
	}
	if !ran {
		t.Fatal("Do did not run fn")
	}
}

// TestDoCapturesTheValueVerbatim uses the shape that matters: a Warren refusal,
// where the panic value IS the diagnostic — several paragraphs of it — and
// summarising it would destroy the only thing the reader needs.
func TestDoCapturesTheValueVerbatim(t *testing.T) {
	t.Parallel()

	const refusal = `warren: app.Transactional composed outside app.Retrying

    app.Chain(h, app.Transactional(uow), app.Retrying(3))

  One transaction would wrap every retry attempt, so a failed attempt's
  staged writes commit alongside the next one's.

  Write it the other way round.`

	c := panics.Do(func() { panic(refusal) })
	if c == nil {
		t.Fatal("Do returned nil for a panicking fn")
	}
	got, ok := c.Value.(string)
	if !ok {
		t.Fatalf("the panic value changed type: %T", c.Value)
	}
	if got != refusal {
		t.Errorf("the panic value was not reproduced verbatim:\ngot:\n%s\nwant:\n%s", got, refusal)
	}
	// And it survives rendering, line for line.
	block := c.Diagnostic("a refusal", "detail.").Error()
	for _, line := range strings.Split(refusal, "\n") {
		if line == "" {
			continue
		}
		if !strings.Contains(block, line) {
			t.Errorf("the rendered block dropped a line of the panic value: %q\n%s", line, block)
		}
	}
}

// TestFramesDropTheMachinery is C0, at depth. The panic is raised through
// reflect, inside a goroutine Warren-shaped code spawned, so the raw stack
// really does carry reflect frames, runtime frames, a "created by" line and
// this package's own plumbing. None of them may reach a reader.
func TestFramesDropTheMachinery(t *testing.T) {
	t.Parallel()

	var c *panics.Caught
	done := make(chan struct{})
	go func() {
		defer close(done)
		c = panics.Do(func() { reflect.ValueOf(panickingHelper).Call(nil) })
	}()
	<-done

	if c == nil {
		t.Fatal("Do returned nil for a panicking fn")
	}
	if len(c.Frames) == 0 {
		t.Fatal("every frame was filtered away; the stack is unusable")
	}
	block := c.Diagnostic("it panicked", "detail.").Error()
	for _, noise := range []string{
		"go.uber.org/dig", // invariant 2 — a dig FRAME leaks dig
		"runtime.",
		"reflect.",
		"panic(",
		"created by ",
	} {
		if strings.Contains(block, noise) {
			t.Errorf("the diagnostic shows machinery %q:\n%s", noise, block)
		}
	}
	// The containment plumbing's own frames, checked on the FUNCTION rather
	// than on the block: this test file lives in internal/panics, so its own
	// file paths carry that string and would answer the question falsely.
	for _, f := range c.Frames {
		if strings.HasPrefix(f.Func, "panics.") {
			t.Errorf("the diagnostic shows this package's own frame %q:\n%s", f.Func, block)
		}
	}
}

func TestFramesStartAtTheCode(t *testing.T) {
	t.Parallel()

	c := panics.Do(panickingHelper)
	if c == nil {
		t.Fatal("Do returned nil for a panicking fn")
	}
	if len(c.Frames) == 0 {
		t.Fatal("no frames were kept")
	}
	if got, want := c.Frames[0].Func, "panics_test.panickingHelper"; got != want {
		t.Errorf("Frames[0].Func = %q, want %q\nframes: %+v", got, want, c.Frames)
	}
	if !strings.Contains(c.Frames[0].At, "panics_test.go:") {
		t.Errorf("Frames[0].At does not name this file: %q", c.Frames[0].At)
	}
}

// TestDiagnosticHidesTheCallersPlumbing covers the hide parameter: a caller's
// own containment frames are never the answer to "where did this come from".
func TestDiagnosticHidesTheCallersPlumbing(t *testing.T) {
	t.Parallel()

	c := panics.Do(panickingHelper)
	if c == nil {
		t.Fatal("Do returned nil for a panicking fn")
	}
	shown := c.Diagnostic("it panicked", "detail.").Error()
	if !strings.Contains(shown, "panics_test.panickingHelper") {
		t.Fatalf("the frame this test hides was not there to hide:\n%s", shown)
	}
	hidden := c.Diagnostic("it panicked", "detail.", "github.com/MerseniBilel/warren/internal/panics_test.").Error()
	if strings.Contains(hidden, "panics_test.") {
		t.Errorf("hide did not drop the named package's frames:\n%s", hidden)
	}
	// The prefix is the caller's own import path and nothing else: the frames
	// below it survive, or "hide" would be a licence to blank the section.
	if !strings.Contains(hidden, "testing.tRunner") {
		t.Errorf("hide dropped a frame it was not given:\n%s", hidden)
	}
}

// TestPassthroughRepanics is http.ErrAbortHandler's clause: net/http raises it
// by design when a client goes away mid-write, and the HTTP edge re-panics it
// deliberately.
func TestPassthroughRepanics(t *testing.T) {
	t.Parallel()

	sentinel := &struct{ name string }{"abort"}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("the passthrough value did not escape Do")
		}
		if r != any(sentinel) {
			t.Fatalf("a different value escaped: %v", r)
		}
	}()
	if c := panics.Do(func() { panic(sentinel) }, sentinel); c != nil {
		t.Fatalf("Do contained a passthrough value: %v", c.Value)
	}
}

// TestPassthroughDoesNotSwallowTheRest — a passthrough list contains one value
// and everything else is still caught, including a value whose type cannot be
// compared at all.
func TestPassthroughDoesNotSwallowTheRest(t *testing.T) {
	t.Parallel()

	sentinel := &struct{ name string }{"abort"}
	c := panics.Do(func() { panic("something else") }, sentinel)
	if c == nil {
		t.Fatal("Do let a non-passthrough panic escape")
	}
	// An uncomparable value — comparing two interfaces holding the same
	// uncomparable dynamic type panics, and a panic raised while containing a
	// panic is the worst failure this package could have.
	c = panics.Do(func() { panic([]string{"uncomparable"}) }, []string{"uncomparable"})
	if c == nil {
		t.Fatal("Do let an uncomparable panic value escape")
	}
}

func TestDiagnosticRendersWithNoFrames(t *testing.T) {
	t.Parallel()

	c := &panics.Caught{Value: "no frames here"}
	got := c.Diagnostic("it panicked", "The first paragraph.\n\nThe second paragraph.").Error()
	want := "✗ it panicked\n\n    no frames here\n\n  The first paragraph.\n\n  The second paragraph."
	if got != want {
		t.Errorf("rendered block:\ngot:\n%q\nwant:\n%q", got, want)
	}
	if strings.Contains(got, "Where it came from") {
		t.Errorf("an empty frame section was printed:\n%s", got)
	}
}

// TestDiagnosticLayout pins the block shape the three call sites inherit.
func TestDiagnosticLayout(t *testing.T) {
	t.Parallel()

	c := &panics.Caught{
		Value: "send on closed channel",
		Frames: []panics.Frame{
			{Func: "kafka.(*consumer).stop", At: "/svc/consumer.go:88"},
		},
	}
	got := c.Diagnostic("lifecycle hook panicked", "Hook \"kafka\" panicked during OnStop.").Error()
	want := "✗ lifecycle hook panicked\n\n" +
		"    send on closed channel\n\n" +
		"  Hook \"kafka\" panicked during OnStop.\n\n" +
		"  Where it came from:\n\n" +
		"    kafka.(*consumer).stop\n" +
		"        /svc/consumer.go:88"
	if got != want {
		t.Errorf("rendered block:\ngot:\n%s\n\nwant:\n%s", got, want)
	}
}
