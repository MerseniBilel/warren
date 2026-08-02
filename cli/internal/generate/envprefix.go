package generate

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// envPrefix finds the environment-variable prefix this project's config
// module was built with, so a generated boot test can set the variables the
// app actually requires.
//
// It is read out of internal/platform/module.go rather than derived from
// the app's name, because the two can differ — `warren new` takes --dir and
// --module independently — and a boot test that fails for want of the wrong
// variable teaches people to delete the boot test.
//
// A project that does not follow the scaffold's shape yields "", and the
// generated test simply sets nothing.
func envPrefix(dir string) string {
	src, err := os.ReadFile(filepath.Join(dir, "internal/platform/module.go"))
	if err != nil {
		return ""
	}
	f, err := parser.ParseFile(token.NewFileSet(), "module.go", src, 0)
	if err != nil {
		return ""
	}

	var prefix string
	ast.Inspect(f, func(n ast.Node) bool {
		if prefix != "" {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		// Match on the selector alone: the config package is imported under
		// whatever alias the scaffold chose.
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "WithEnvPrefix" {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if unquoted, uerr := strconv.Unquote(lit.Value); uerr == nil {
			prefix = strings.TrimSuffix(unquoted, "_")
		}
		return false
	})
	return prefix
}
