// Package astedit inserts code into an existing Go file without reprinting
// it.
//
// The insertion point is located through the AST — never a regular
// expression, never a marker comment — and then the new text is SPLICED
// into the original bytes. The file is never re-rendered, so comments,
// blank lines, and the author's own formatting survive by construction
// rather than by a decoration model that has to be kept in step with the
// language.
//
// This is the model x/tools/go/analysis uses for its suggested fixes
// (TextEdit{Pos, End, NewText}), and it is why this package needs no
// third-party dependency.
package astedit

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
)

// AddArgument adds ident as the last argument of the call to fn — for
// example warren.Providers — inside the file's warren.NewModule call. When
// no such call exists, it is created as a new option of NewModule.
//
// It is idempotent: an ident already present is left alone and the file is
// returned unchanged, which is what makes re-running a generator a no-op.
func AddArgument(src []byte, fn, ident string) ([]byte, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "module.go", src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("astedit: %w", err)
	}

	newModule, err := findModuleCall(f)
	if err != nil {
		return nil, err
	}

	// Search for the option INSIDE the module being edited, never across the
	// whole file. A file that declares two modules would otherwise have its
	// provider spliced into whichever one happens to appear first — a
	// mis-wiring that compiles, passes the other module's boot test, and
	// surfaces only as a resolution failure at run time.
	if call := findCallIn(newModule, fn); call != nil {
		if hasArg(fset, src, call, ident) {
			return src, nil // already there: a no-op, by design
		}
		return spliceIntoCall(fset, src, call, ident)
	}
	return spliceNewOption(fset, src, newModule, fn, ident)
}

// AddCallArgument adds ident as the last argument of the call to fn — for
// example warren.New in a main package — and errors when there is no such
// call. Unlike AddArgument it creates nothing: the call has to exist,
// because there is no module declaration to hang a new option off.
//
// It is idempotent for the same reason.
func AddCallArgument(src []byte, fn, ident string) ([]byte, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("astedit: %w", err)
	}
	call := findCall(f, fn)
	if call == nil {
		return nil, diagnostic(fmt.Sprintf(
			"✗ no %s(...) call found\n\n    There is nothing here to add %s to.\n\n"+
				"  Run this from a project scaffolded by `warren new`.", fn, ident))
	}
	if hasArg(fset, src, call, ident) {
		return src, nil
	}
	return spliceIntoCall(fset, src, call, ident)
}

// hasArg reports whether the call already passes exactly this expression.
func hasArg(fset *token.FileSet, src []byte, call *ast.CallExpr, ident string) bool {
	for _, arg := range call.Args {
		if exprText(fset, src, arg) == ident {
			return true
		}
	}
	return false
}

// AddImport adds an import path if it is absent, and returns the file
// unchanged if it is present.
func AddImport(src []byte, path string) ([]byte, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "module.go", src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("astedit: %w", err)
	}
	for _, spec := range f.Imports {
		if p, uerr := strconv.Unquote(spec.Path.Value); uerr == nil && p == path {
			return src, nil
		}
	}
	if len(f.Imports) == 0 {
		return nil, diagnostic("✗ no import block\n\n    This file has no imports to add to.")
	}

	// `import "x"` — legal Go, and not what the scaffold writes, but this
	// tool meets files it did not author. Turn it into a block, because
	// there is nowhere to splice a second path otherwise.
	if decl := importDecl(f); decl != nil && !decl.Lparen.IsValid() {
		start := fset.Position(decl.Pos()).Offset
		end := fset.Position(decl.End()).Offset
		only := strconv.Quote(mustUnquote(f.Imports[0].Path.Value))
		block := "import (\n\t" + only + "\n\t" + strconv.Quote(path) + "\n)"
		return spliceRange(src, start, end, block)
	}

	// Splice after the last import, matching its indentation. The offset has
	// to advance to the END OF THE LINE: an import spec ends at its path, so
	// splicing there lands between a path and its own trailing comment, and
	// the comment then documents the wrong import.
	last := f.Imports[len(f.Imports)-1]
	offset, _ := afterLast(src, fset.Position(last.End()).Offset)
	indent := indentOf(src, fset.Position(last.Pos()).Offset)
	insert := "\n" + indent + strconv.Quote(path)
	return splice(src, offset, insert)
}

// importDecl returns the file's import declaration, if it has exactly one.
func importDecl(f *ast.File) *ast.GenDecl {
	var found *ast.GenDecl
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.IMPORT {
			continue
		}
		if found != nil {
			return nil // several: leave them alone
		}
		found = gen
	}
	return found
}

func mustUnquote(lit string) string {
	if s, err := strconv.Unquote(lit); err == nil {
		return s
	}
	return lit
}

// spliceIntoCall appends an argument to an existing call.
func spliceIntoCall(fset *token.FileSet, src []byte, call *ast.CallExpr, ident string) ([]byte, error) {
	if len(call.Args) == 0 {
		// An empty call: put the argument between the parentheses.
		offset := fset.Position(call.Lparen).Offset + 1
		return splice(src, offset, ident)
	}
	last := call.Args[len(call.Args)-1]
	offset := fset.Position(last.End()).Offset

	// A trailing comma already there means the call is multi-line; otherwise
	// we add one.
	offset, prefix := afterLast(src, offset)
	indent := indentOf(src, fset.Position(last.Pos()).Offset)
	return splice(src, offset, prefix+"\n"+indent+ident+",")
}

// spliceNewOption creates a missing option inside warren.NewModule.
func spliceNewOption(fset *token.FileSet, src []byte, newModule *ast.CallExpr, fn, ident string) ([]byte, error) {
	if len(newModule.Args) == 0 {
		return nil, diagnostic("✗ warren.NewModule has no arguments to add to")
	}
	last := newModule.Args[len(newModule.Args)-1]
	offset := fset.Position(last.End()).Offset
	offset, prefix := afterLast(src, offset)
	indent := indentOf(src, fset.Position(last.Pos()).Offset)
	insert := fmt.Sprintf("%s\n%s%s(\n%s\t%s,\n%s),", prefix, indent, fn, indent, ident, indent)
	return splice(src, offset, insert)
}

// afterLast moves an offset past the last argument's trailing comma AND
// past any comment on the same line, so an inserted argument lands on its
// own line and never steals the previous one's comment. It reports whether
// a comma still has to be written.
func afterLast(src []byte, offset int) (int, string) {
	prefix := ","
	rest := src[offset:]
	if trimmed := bytes.TrimLeft(rest, " \t"); len(trimmed) > 0 && trimmed[0] == ',' {
		offset += len(rest) - len(trimmed) + 1
		prefix = ""
	}
	// Advance to the end of the line: a trailing // comment belongs to the
	// argument above it.
	if nl := bytes.IndexByte(src[offset:], '\n'); nl >= 0 {
		tail := strings.TrimSpace(strings.TrimSuffix(string(src[offset:offset+nl]), "\r"))
		if tail == "" || strings.HasPrefix(tail, "//") || strings.HasPrefix(tail, "/*") {
			offset += nl
		}
	}
	return offset, prefix
}

// splice inserts text at a byte offset and re-formats — format.Source only
// normalises whitespace, so the surrounding file is untouched.
func splice(src []byte, offset int, text string) ([]byte, error) {
	return spliceRange(src, offset, offset, text)
}

// spliceRange replaces src[start:end] with text and re-formats.
//
// format.Source normalises line endings to LF, so a CRLF file is restored
// afterwards. Without that, a one-line wiring edit on a Windows checkout
// becomes a whole-file diff — which contradicts the promise this package
// exists to keep.
func spliceRange(src []byte, start, end int, text string) ([]byte, error) {
	out := make([]byte, 0, len(src)+len(text))
	out = append(out, src[:start]...)
	out = append(out, text...)
	out = append(out, src[end:]...)

	crlf := bytes.Contains(src, []byte("\r\n"))
	if crlf {
		out = bytes.ReplaceAll(out, []byte("\r\n"), []byte("\n"))
	}
	formatted, err := format.Source(out)
	if err != nil {
		return nil, fmt.Errorf("astedit: the edit produced invalid Go: %w", err)
	}
	if crlf {
		formatted = bytes.ReplaceAll(formatted, []byte("\n"), []byte("\r\n"))
	}
	return formatted, nil
}

// findModuleCall returns the warren.NewModule call this file's edits belong
// to: the one inside `var Module = ...`, which is the convention every
// scaffolded and generated module.go follows.
//
// A file with exactly one such call is unambiguous whatever it is named. A
// file with several and no `Module` is an error rather than a guess —
// picking wrong wires a provider into the wrong container.
func findModuleCall(f *ast.File) (*ast.CallExpr, error) {
	var all []*ast.CallExpr
	ast.Inspect(f, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && isCallTo(call, "warren.NewModule") {
			all = append(all, call)
		}
		return true
	})

	switch len(all) {
	case 0:
		return nil, diagnostic(
			"✗ no module declaration found\n\n    This file has no warren.NewModule(...) call to add to.\n\n" +
				"  Generators wire new code into a module's declaration. Run this from a\n" +
				"  project scaffolded by `warren new`, or create the module first with\n" +
				"  `warren g module <name>`.")
	case 1:
		return all[0], nil
	}

	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) == 0 || value.Names[0].Name != "Module" {
				continue
			}
			for _, call := range all {
				if within(value, call) {
					return call, nil
				}
			}
		}
	}
	return nil, diagnostic(fmt.Sprintf(
		"✗ this file declares %d modules\n\n    None of them is assigned to a variable named Module, so there is\n"+
			"    no way to tell which one new code belongs to — and wiring it into\n"+
			"    the wrong container is a failure that only shows up at run time.\n\n"+
			"  Name the feature's module `Module`, or add the provider by hand.", len(all)))
}

// within reports whether the call lies inside the node's source range.
func within(outer ast.Node, call *ast.CallExpr) bool {
	return call.Pos() >= outer.Pos() && call.End() <= outer.End()
}

// findCallIn returns the first call to a dotted name within a subtree.
func findCallIn(root ast.Node, dotted string) *ast.CallExpr {
	var found *ast.CallExpr
	ast.Inspect(root, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		if call, ok := n.(*ast.CallExpr); ok && isCallTo(call, dotted) {
			found = call
			return false
		}
		return true
	})
	return found
}

// isCallTo reports whether the call is to a dotted name like
// "warren.Providers".
func isCallTo(call *ast.CallExpr, dotted string) bool {
	pkg, name, ok := strings.Cut(dotted, ".")
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}

// findCall returns the first call to a dotted name like "warren.Providers".
func findCall(f *ast.File, dotted string) *ast.CallExpr {
	pkg, name, ok := strings.Cut(dotted, ".")
	if !ok {
		return nil
	}
	var found *ast.CallExpr
	ast.Inspect(f, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		call, isCall := n.(*ast.CallExpr)
		if !isCall {
			return true
		}
		sel, isSel := call.Fun.(*ast.SelectorExpr)
		if !isSel || sel.Sel.Name != name {
			return true
		}
		if id, isID := sel.X.(*ast.Ident); isID && id.Name == pkg {
			found = call
			return false
		}
		return true
	})
	return found
}

// exprText returns an expression's source text, so an argument can be
// compared with the identifier a generator wants to add.
func exprText(fset *token.FileSet, src []byte, e ast.Expr) string {
	start := fset.Position(e.Pos()).Offset
	end := fset.Position(e.End()).Offset
	if start < 0 || end > len(src) || start > end {
		return ""
	}
	return string(src[start:end])
}

// indentOf returns the leading whitespace of the line containing offset.
func indentOf(src []byte, offset int) string {
	start := bytes.LastIndexByte(src[:offset], '\n') + 1
	line := src[start:offset]
	return string(line[:len(line)-len(bytes.TrimLeft(line, " \t"))])
}

type diagnostic string

func (d diagnostic) Error() string { return string(d) }
