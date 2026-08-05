package warren_test

import (
	"context"
	"strings"
	"testing"

	"github.com/MerseniBilel/warren"
)

type thing struct{ n int }

// TestCannotReExportAnImportedType pins the diagnostic for the facade shape:
// a platform module that imports a driver module and tries to export its
// ports onward, so features import one thing. Warren forwards exports one
// hop and no further, and the generic "add the constructor" advice sends the
// reader looking for a constructor that already exists next door.
func TestCannotReExportAnImportedType(t *testing.T) {
	driver := warren.NewModule("driver",
		warren.Providers(func() *thing { return &thing{n: 7} }),
		warren.Exports[*thing](),
	)
	platform := warren.NewModule("platform",
		warren.Imports(driver),
		warren.Exports[*thing](),
	)
	err := warren.New(driver, platform).Start(context.Background())
	if err == nil {
		t.Fatal("re-export booted; it must not")
	}
	for _, want := range []string{
		"cannot re-export an imported type",
		`imports`,
		`"driver"`,
		"sync.OnceValue",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("diagnostic omits %q:\n%s", want, err)
		}
	}
	// The generic diagnostic must not be the one that fires here.
	if strings.Contains(err.Error(), "none of its providers returns it") {
		t.Errorf("got the export-without-provider message instead:\n%s", err)
	}
}

// TestExportWithoutProviderStillFires guards the other side: a module that
// exports something nothing provides anywhere still gets the original advice.
func TestExportWithoutProviderStillFires(t *testing.T) {
	m := warren.NewModule("lonely", warren.Exports[*thing]())
	err := warren.New(m).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "none of its providers returns it") {
		t.Fatalf("want export-without-provider, got: %v", err)
	}
}
