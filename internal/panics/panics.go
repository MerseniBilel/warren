// Package panics contains one recovered panic and renders it as a Warren
// diagnostic. It exists so that the frame-filtering rules — which are the
// difference between a diagnostic and a stack trace, and which enforce
// invariant 2 — have exactly one implementation.
//
// Three places invoke user code during boot or shutdown: warren/di calls a
// constructor, warren/lifecycle calls a hook, and the bootstrapper calls a
// controller's Register. A panic in any of them used to be a raw Go dump and
// exit 2; contained, it is a Warren error block and exit 1, and the rollback
// and drain guarantees the framework makes still hold. A second copy of these
// rules in each of the three would drift within a month, and the invariant
// they enforce would then hold in one place out of three.
//
// What this package does NOT contain is as fixed as what it does: it is
// recover-shaped, so stack exhaustion, out of memory, a concurrent map write
// and runtime.Goexit are outside it, and a panic in a goroutine the user
// spawned is theirs to recover. "Contained" is the word; "cannot crash" is
// not.
package panics

import (
	"fmt"
	"runtime/debug"
	"strings"
)

// Frame is one stack frame a reader can act on: the function, and the
// file:line it was at.
type Frame struct {
	Func string // "application.NewRegisterUserHandler"
	At   string // "/home/me/svc/internal/modules/user/application/register.go:24"

	// full is the frame's package-qualified function name, before Func dropped
	// the module path. Diagnostic's hide prefixes are import paths — that is
	// how a caller names its own package unambiguously — so the match has to
	// happen against the name the caller can actually write down.
	full string
}

// Caught is a recovered panic together with the stack at the point it was
// raised, reduced to the frames a reader can act on.
//
// Frames[0] is the code that panicked. The panic machinery, runtime and
// reflect frames, go.uber.org/dig's frames (invariant 2), and this package's
// own are never present.
type Caught struct {
	// Value is the panic value, untouched. For a Warren refusal the value IS
	// the diagnostic — several paragraphs of it — so it is reproduced
	// verbatim and never summarised.
	Value  any
	Frames []Frame
}

// Do runs fn and returns nil, or the panic fn raised.
//
// It re-panics with http.ErrAbortHandler-shaped values it is told to let
// through: see the passthrough parameter. Nothing else escapes.
//
// It is a function taking fn rather than a deferred helper for one reason: a
// deferred helper has to be written correctly at each call site — one
// forgotten & away from silence — and lifecycle's call site is inside a
// goroutine where the result has to cross a channel anyway. Do cannot be
// misused: it either returns nil or it returns the panic.
func Do(fn func(), passthrough ...any) (caught *Caught) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		for _, p := range passthrough {
			if identical(r, p) {
				// Deliberately re-raised, on the original goroutine, with the
				// original value: net/http raises http.ErrAbortHandler by
				// design and the HTTP edge is built to see it.
				panic(r)
			}
		}
		caught = &Caught{Value: r, Frames: frames(string(debug.Stack()))}
	}()
	fn()
	return nil
}

// identical reports whether two panic values are the same value.
//
// Comparing two interface values panics when their dynamic types are
// identical and not comparable — a slice, a map, a func — so the comparison is
// itself contained. A panic raised while containing a panic is the worst
// failure this package could have.
func identical(a, b any) (eq bool) {
	defer func() { _ = recover() }()
	return a == b
}

// Diagnostic renders the caught panic as a Warren error block:
//
//	✗ <headline>
//
//	    <Value, verbatim, every line indented four spaces>
//
//	  <detail>
//
//	  Where it came from:
//
//	    <Frame.Func>
//	        <Frame.At>
//
// hide names additional function-name prefixes to drop — a caller's own
// containment plumbing, which is never the answer to "where did this come
// from". They are import-path prefixes ending in a dot
// ("github.com/MerseniBilel/warren/di."), so that one package's frames are
// dropped and a package whose path merely starts the same way keeps its own.
// The universal drops are built in and are not the caller's to choose.
//
// A block with no visible frames prints without the "Where it came from"
// section rather than printing an empty one.
//
// The returned error's message is the rendered block. It does not wrap the
// panic value: a panic value is not an error, and errors.As reaching into one
// would make an arbitrary user value part of Warren's error contract.
func (c *Caught) Diagnostic(headline, detail string, hide ...string) error {
	var b strings.Builder
	b.WriteString("✗ " + headline + "\n\n")
	for _, line := range strings.Split(strings.TrimRight(fmt.Sprint(c.Value), "\n"), "\n") {
		b.WriteString("    " + line + "\n")
	}
	b.WriteString("\n")
	for _, line := range strings.Split(strings.TrimRight(detail, "\n"), "\n") {
		if line == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString("  " + line + "\n")
	}
	if shown := c.visible(hide); len(shown) > 0 {
		b.WriteString("\n  Where it came from:\n\n")
		for _, f := range shown {
			b.WriteString("    " + f.Func + "\n        " + f.At + "\n")
		}
	}
	return &diagnostic{text: strings.TrimRight(b.String(), "\n")}
}

// visible drops the caller's own plumbing from the already-filtered frames.
func (c *Caught) visible(hide []string) []Frame {
	if len(hide) == 0 {
		return c.Frames
	}
	out := make([]Frame, 0, len(c.Frames))
	for _, f := range c.Frames {
		drop := false
		for _, prefix := range hide {
			if strings.HasPrefix(f.full, prefix) {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, f)
		}
	}
	return out
}

// diagnostic is the rendered block as an error. The text is the contract; it
// wraps nothing.
type diagnostic struct{ text string }

func (d *diagnostic) Error() string { return d.text }

// frames turns debug.Stack's output into the frames a user can act on.
//
// It drops four kinds of noise: the panic machinery (runtime.gopanic and the
// deferred recover that captured this), dig's frames — which invariant 2
// forbids showing — reflect's, which are how a container calls a constructor
// at all and say nothing about any particular one, and this package's own.
// What is left starts at the code that actually panicked.
//
// Warren's OTHER frames — di, lifecycle, the root package — are deliberately
// kept here and dropped by Diagnostic's hide instead: which of them is
// plumbing depends on who placed the recover, and a filter that guessed would
// hide the frame the reader needed.
func frames(stack string) []Frame {
	lines := strings.Split(stack, "\n")
	var out []Frame
	// Frames come in pairs: a function line, then a tab-indented file:line.
	for i := 0; i+1 < len(lines); i++ {
		fn := lines[i]
		at := strings.TrimSpace(lines[i+1])
		if fn == "" || !strings.HasPrefix(lines[i+1], "\t") {
			continue
		}
		i++ // the file:line belongs to this frame, never to the next
		switch {
		case strings.HasPrefix(fn, "go.uber.org/dig"), strings.Contains(at, "go.uber.org/dig"):
		case strings.HasPrefix(fn, "panic("), strings.HasPrefix(fn, "runtime/debug.Stack"):
		case strings.HasPrefix(fn, "runtime."), strings.HasPrefix(fn, "created by "):
		case strings.HasPrefix(fn, selfPrefix):
		case strings.HasPrefix(fn, "reflect."):
		default:
			// The argument list and the +0x.. offset are compiler detail that
			// helps nobody reading a boot failure. Cut at the LAST paren and
			// only when one closes the line: a method's frame is printed
			// "warren.(*App).Start.func1(0x14000)", whose FIRST paren opens
			// the receiver — cutting there left the frame reading "warren."
			// and nothing else.
			if strings.HasSuffix(fn, ")") {
				if p := strings.LastIndexByte(fn, '('); p > 0 {
					fn = fn[:p]
				}
			}
			if p := strings.LastIndex(at, " +0x"); p > 0 {
				at = at[:p]
			}
			out = append(out, Frame{Func: shortFuncName(fn), At: at, full: fn})
		}
	}
	return out
}

// selfPrefix is this package's own import path. A frame of the containment
// plumbing is never the answer to "where did this come from", and unlike the
// three call sites' packages there is no case in which it is.
const selfPrefix = "github.com/MerseniBilel/warren/internal/panics."

// shortFuncName drops the module path from a stack frame's function, leaving
// the package and the function — "user/application.NewRegisterUserHandler"
// rather than 60 characters of forge, owner and repository that are the same
// on every line.
func shortFuncName(fn string) string {
	dot := strings.LastIndexByte(fn, '.')
	if dot < 0 {
		return fn
	}
	pkg := fn[:dot]
	if slash := strings.LastIndexByte(pkg, '/'); slash >= 0 {
		pkg = pkg[slash+1:]
	}
	return pkg + fn[dot:]
}
