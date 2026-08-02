package astedit

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func show(t *testing.T, name string, src string, run func([]byte) ([]byte, error)) {
	t.Helper()
	out, err := run([]byte(src))
	t.Logf("\n==== %s ====\nIN:\n%s\n----\n", name, src)
	if err != nil {
		t.Logf("ERR: %v\n", err)
		return
	}
	t.Logf("OUT:\n%s\n----\n", out)
	if _, perr := parser.ParseFile(token.NewFileSet(), "x.go", out, parser.ParseComments); perr != nil {
		t.Errorf("%s: PRODUCED INVALID GO: %v", name, perr)
	}
	if strings.Count(string(out), "//") != strings.Count(src, "//") {
		t.Errorf("%s: COMMENT COUNT CHANGED %d -> %d", name, strings.Count(src, "//"), strings.Count(string(out), "//"))
	}
}

const hdr = `package m

import (
	"github.com/MerseniBilel/warren"
	"x/app"
)

`

func TestHostileOneLine(t *testing.T) {
	show(t, "one-line", hdr+`var M = warren.NewModule("m", warren.Providers(app.A, app.B))
`, func(b []byte) ([]byte, error) { return AddArgument(b, "warren.Providers", "app.C") })
}

func TestHostileTrailingComment(t *testing.T) {
	show(t, "trailing-comment", hdr+`var M = warren.NewModule("m",
	warren.Providers(
		app.A, // the first one
	),
)
`, func(b []byte) ([]byte, error) { return AddArgument(b, "warren.Providers", "app.C") })
}

func TestHostileBlockComment(t *testing.T) {
	show(t, "block-comment", hdr+`var M = warren.NewModule("m",
	warren.Providers(
		app.A, /* keep me with A */
	),
)
`, func(b []byte) ([]byte, error) { return AddArgument(b, "warren.Providers", "app.C") })
}

func TestHostileCommentBeforeParen(t *testing.T) {
	show(t, "comment-before-paren", hdr+`var M = warren.NewModule("m",
	warren.Providers(
		app.A,
		// TODO: add the rest
	),
)
`, func(b []byte) ([]byte, error) { return AddArgument(b, "warren.Providers", "app.C") })
}

func TestHostileTwoModules(t *testing.T) {
	show(t, "two-modules", hdr+`var Other = warren.NewModule("other",
	warren.Providers(
		app.Other,
	),
)

var M = warren.NewModule("m",
	warren.Providers(
		app.A,
	),
)
`, func(b []byte) ([]byte, error) { return AddArgument(b, "warren.Providers", "app.C") })
}

func TestHostileFuncLit(t *testing.T) {
	show(t, "func-literal", hdr+`var M = warren.NewModule("m",
	warren.Providers(
		func() *app.T { return nil },
	),
)
`, func(b []byte) ([]byte, error) { return AddArgument(b, "warren.Providers", "app.C") })
}

func TestHostileCompositeLit(t *testing.T) {
	show(t, "composite-literal", hdr+`var M = warren.NewModule("m",
	warren.Providers(
		app.Cfg{
			N: 1,
		},
	),
)
`, func(b []byte) ([]byte, error) { return AddArgument(b, "warren.Providers", "app.C") })
}

func TestHostileCRLF(t *testing.T) {
	src := strings.ReplaceAll(hdr+`var M = warren.NewModule("m",
	warren.Providers(
		app.A,
	),
)
`, "\n", "\r\n")
	show(t, "crlf", src, func(b []byte) ([]byte, error) { return AddArgument(b, "warren.Providers", "app.C") })
}

func TestHostileInsideIf(t *testing.T) {
	show(t, "inside-if", hdr+`func New(dev bool) warren.Module {
	if dev {
		return warren.NewModule("m", warren.Providers(app.Dev))
	}
	return warren.NewModule("m", warren.Providers(app.A))
}
`, func(b []byte) ([]byte, error) { return AddArgument(b, "warren.Providers", "app.C") })
}

func TestHostileBuildTags(t *testing.T) {
	show(t, "build-tags", `//go:build !prod

package m

import (
	"github.com/MerseniBilel/warren"
	"x/app"
)

var M = warren.NewModule("m",
	warren.Providers(
		app.A,
	),
)
`, func(b []byte) ([]byte, error) { return AddArgument(b, "warren.Providers", "app.C") })
}

func TestHostileNoProvidersNoNewModule(t *testing.T) {
	show(t, "no-newmodule", hdr+`var M = app.Thing()
`, func(b []byte) ([]byte, error) { return AddArgument(b, "warren.Providers", "app.C") })
}

func TestHostileImportAfterComment(t *testing.T) {
	show(t, "import-trailing-comment", `package m

import (
	"github.com/MerseniBilel/warren" // the framework
)

var M = warren.NewModule("m", warren.Imports())
`, func(b []byte) ([]byte, error) { return AddImport(b, "x/app") })
}

func TestHostileSingleImport(t *testing.T) {
	show(t, "single-import-no-parens", `package m

import "github.com/MerseniBilel/warren"

var M = warren.NewModule("m", warren.Imports())
`, func(b []byte) ([]byte, error) { return AddImport(b, "x/app") })
}

func TestHostileDotImport(t *testing.T) {
	show(t, "no-imports-at-all", `package m

var M = 1
`, func(b []byte) ([]byte, error) { return AddImport(b, "x/app") })
}
