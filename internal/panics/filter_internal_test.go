package panics

import (
	"strings"
	"testing"
)

// digStack is a real debug.Stack() capture, taken from a panic raised by a
// user constructor that dig invoked, reduced to the frames that matter and
// with the module-cache paths left exactly as they print.
//
// It is written down rather than produced, because producing it would mean
// importing go.uber.org/dig here — and invariant 2 says dig is imported by
// warren/di and by nothing else. The frames this filter must never show are
// the only reason the filter exists, so they are pinned as data.
const digStack = `goroutine 1 [running]:
runtime/debug.Stack()
	/usr/local/go/src/runtime/debug/stack.go:26 +0x64
github.com/MerseniBilel/warren/internal/panics.Do.func1()
	/svc/internal/panics/panics.go:71 +0x40
panic({0x1023a4e00?, 0x1024b1d70?})
	/usr/local/go/src/runtime/panic.go:783 +0x120
user/application.NewRegisterUserHandler(...)
	/home/me/svc/internal/modules/user/application/register.go:24 +0x2c
reflect.Value.call({0x1023b8f60?, 0x102490a18?, 0x13?}, {0x102290a4b, 0x4}, {0x14000123e28, 0x1, 0x1})
	/usr/local/go/src/reflect/value.go:584 +0xcb0
reflect.Value.Call({0x1023b8f60?, 0x102490a18?, 0x1400012bd18?}, {0x14000123e28?, 0x1023a1e40?, 0x1400012bd58?})
	/usr/local/go/src/reflect/value.go:368 +0x94
go.uber.org/dig.(*constructorNode).Call(0x14000138180, {0x1023bb2a0, 0x14000116100})
	/Users/me/go/pkg/mod/go.uber.org/dig@v1.19.0/constructor.go:187 +0x2e4
go.uber.org/dig.paramSingle.Build({{0x102290a52, 0x0}, 0x0, {0x1023b8f60, 0x102490a18}}, {0x1023bb2a0, 0x14000116100})
	/Users/me/go/pkg/mod/go.uber.org/dig@v1.19.0/param.go:283 +0x2c8
github.com/MerseniBilel/warren/di.(*container).Invoke(0x14000110080, {0x1023b8ec0, 0x1024909f8})
	/svc/di/container.go:175 +0x1f8
github.com/MerseniBilel/warren.(*App).Start(0x140001220c0, {0x1023bb1e0, 0x1024b1d60})
	/svc/app.go:241 +0x5a4
main.main()
	/home/me/svc/cmd/svc/main.go:18 +0x3c
created by github.com/MerseniBilel/warren/lifecycle.runHook in goroutine 1
	/svc/lifecycle/lifecycle.go:269 +0x9c
`

func TestFilterDropsEveryDigFrame(t *testing.T) {
	t.Parallel()

	got := frames(digStack)
	if len(got) == 0 {
		t.Fatal("every frame was filtered away")
	}
	if got[0].Func != "application.NewRegisterUserHandler" {
		t.Errorf("Frames[0] is not the user's code: %+v", got)
	}
	for _, f := range got {
		for _, noise := range []string{"go.uber.org/dig", "runtime.", "reflect.", "panic(", "created by ", "internal/panics", "runtime/debug"} {
			if strings.Contains(f.Func, noise) || strings.Contains(f.At, noise) || strings.Contains(f.full, noise) {
				t.Errorf("frame %+v carries machinery %q", f, noise)
			}
		}
	}
}

// TestFilterKeepsWarrensOwnFramesForTheCallerToHide proves the split: the
// universal filter does NOT drop di, lifecycle or warren frames, because
// which of them is plumbing depends on who placed the recover. That is what
// Diagnostic's hide parameter decides, and a filter that guessed would hide a
// frame the reader needed.
func TestFilterKeepsWarrensOwnFramesForTheCallerToHide(t *testing.T) {
	t.Parallel()

	var kept []string
	for _, f := range frames(digStack) {
		kept = append(kept, f.Func)
	}
	want := []string{
		"application.NewRegisterUserHandler",
		"di.(*container).Invoke",
		"warren.(*App).Start",
		"main.main",
	}
	if len(kept) != len(want) {
		t.Fatalf("frames = %q, want %q", kept, want)
	}
	for i := range want {
		if kept[i] != want[i] {
			t.Fatalf("frames = %q, want %q", kept, want)
		}
	}
}

// TestFilterStripsArgumentsAndOffsets pins the two cosmetic cuts: a frame's
// argument list and its +0x offset are compiler detail that helps nobody
// reading a boot failure. The receiver's parenthesis is NOT the cut point —
// cutting there once left frames rendering as the bare word "warren.".
func TestFilterStripsArgumentsAndOffsets(t *testing.T) {
	t.Parallel()

	for _, f := range frames(digStack) {
		if strings.Contains(f.Func, "(0x") || strings.Contains(f.Func, "{0x") {
			t.Errorf("frame keeps its argument list: %q", f.Func)
		}
		if strings.Contains(f.At, " +0x") {
			t.Errorf("frame keeps its offset: %q", f.At)
		}
	}
	got := frames(digStack)
	if got[1].Func != "di.(*container).Invoke" {
		t.Errorf("a method frame lost its receiver: %q", got[1].Func)
	}
	if got[1].At != "/svc/di/container.go:175" {
		t.Errorf("a frame's file:line is wrong: %q", got[1].At)
	}
}
