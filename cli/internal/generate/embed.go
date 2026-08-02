package generate

import (
	"embed"
	"fmt"
	"go/format"
	"strings"
)

//go:embed templates
var templates embed.FS

// gofmt formats generated Go. A template that produces unparseable code is
// a bug in the CLI, and this is where it surfaces — named, at generation
// time — rather than in the user's editor.
func gofmt(src []byte, name string) ([]byte, error) {
	if !strings.HasSuffix(name, ".go.tmpl") {
		return src, nil
	}
	out, err := format.Source(src)
	if err != nil {
		return nil, fmt.Errorf("warren g: template %s produced invalid Go: %w", name, err)
	}
	return out, nil
}
