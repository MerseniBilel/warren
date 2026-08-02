package astedit

import (
	"bytes"
	"strings"
	"testing"
)

func TestCRLFPreserved(t *testing.T) {
	src := strings.ReplaceAll(`package m

import (
	"github.com/MerseniBilel/warren"
	"x/app"
)

var M = warren.NewModule("m",
	warren.Providers(
		app.A,
	),
)
`, "\n", "\r\n")
	out, err := AddArgument([]byte(src), "warren.Providers", "app.C")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("CR in:  %d", bytes.Count([]byte(src), []byte("\r")))
	t.Logf("CR out: %d", bytes.Count(out, []byte("\r")))
}
