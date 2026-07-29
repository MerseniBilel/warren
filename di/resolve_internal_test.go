package di

import (
	"context"
	"reflect"
	"testing"
)

// resolveReturning is the API shape SPEC.md §11.3 rejected: (T, error) rather
// than an out-parameter. It exists only so that the decision rests on a
// measurement instead of an assertion, and it is compared against the real
// [Resolve] in BenchmarkResolveShape.
//
// The claim under test is that returning a value has to materialise a zero T on
// the error path, and that this costs something for a T that is not a pointer.
func resolveReturning[T any](c *Container) (T, error) {
	var zero T

	err := c.resolvable(opResolve)
	if err != nil {
		return zero, err
	}

	want := reflect.TypeFor[T]()

	value, ok := c.instances[want]
	if !ok {
		return zero, errNoProvider(opResolve, want, nil, Site{}, c.nearMiss(want))
	}

	instance, ok := value.(T)
	if !ok {
		return zero, errNoProvider(opResolve, want, nil, Site{}, nil)
	}

	return instance, nil
}

// wide is a value type large enough that materialising a zero of it is not free,
// which is the case §11.3's argument turns on.
type wide struct {
	fields [32]string
}

func newWide() wide {
	var w wide

	w.fields[0] = "warren"

	return w
}

func newPointer() *wide { return &wide{} }

// BenchmarkResolveShape settles SPEC.md §11.3. If the two shapes measure the
// same, the out-parameter has no justification and the nicer signature wins.
func BenchmarkResolveShape(b *testing.B) {
	c := New()
	Provide[wide](c, newWide)
	Provide[*wide](c, newPointer)

	buildErr := c.Build(context.Background())
	if buildErr != nil {
		b.Fatal(buildErr)
	}

	b.Run("out-parameter, value type", func(b *testing.B) {
		var got wide

		b.ReportAllocs()

		for b.Loop() {
			err := Resolve(c, &got)
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("returned, value type", func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			_, err := resolveReturning[wide](c)
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("out-parameter, pointer type", func(b *testing.B) {
		var got *wide

		b.ReportAllocs()

		for b.Loop() {
			err := Resolve(c, &got)
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("returned, pointer type", func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			_, err := resolveReturning[*wide](c)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

// TestIsClosure covers the naming convention memoID depends on: a named function
// is safe to memoise, a closure is not, because every closure from one source
// location shares a code pointer.
func TestIsClosure(t *testing.T) {
	t.Parallel()

	named := reflect.ValueOf(newWide)
	if memoID(named) == 0 {
		t.Error("a named function got no memo identity, so it will be built twice")
	}

	tag := "captured"
	closure := reflect.ValueOf(func() wide {
		var w wide

		w.fields[0] = tag

		return w
	})
	if memoID(closure) != 0 {
		t.Error("a closure got a memo identity, so two closures could be merged")
	}
}
