package astedit

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

// Exposing a generated use case means editing three places in a file the
// generator did not write: a struct, a constructor, and a method body.
// Until these existed `warren g command` printed a twelve-line patch and
// asked the user to make all three by hand, every time — and the file it
// named is one no generator creates.
//
// Each edit is idempotent, because a generator is re-run and a duplicate
// field does not compile. Each refuses a target it cannot find, because a
// splice that silently does nothing leaves a handler nothing serves and
// nothing to explain why.

// AddStructField adds "name typ" as the last field of the named struct.
func AddStructField(src []byte, typeName, name, typ string) ([]byte, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "controller.go", src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("astedit: %w", err)
	}
	st, err := findStruct(f, typeName)
	if err != nil {
		return nil, err
	}
	if hasField(st, name) {
		return src, nil
	}

	// Before the closing brace, on its own line, indented like the fields
	// already there.
	closing := fset.Position(st.Fields.Closing).Offset
	indent := indentOf(src, closing) + "\t"
	if len(st.Fields.List) > 0 {
		last := st.Fields.List[len(st.Fields.List)-1]
		indent = indentOf(src, fset.Position(last.Pos()).Offset)
	}
	return splice(src, lineStart(src, closing), indent+name+" "+typ+"\n")
}

// AddConstructorParam adds "name typ" as the last parameter of fn AND
// "name: name" to the &<typeName>{…} literal it returns.
//
// Both halves are one call because either alone leaves the file
// uncompilable — an unused parameter, or a field assigned from nothing.
func AddConstructorParam(src []byte, fn, typeName, name, typ string) ([]byte, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "controller.go", src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("astedit: %w", err)
	}
	decl := findFunc(f, fn)
	if decl == nil {
		return nil, diagnostic(fmt.Sprintf(
			"astedit: no function %s in this file — the controller's constructor is where a handler is injected", fn))
	}
	if hasParam(decl, name) {
		return src, nil
	}

	// The literal FIRST: splicing the parameter list moves every offset
	// after it, and the literal is always after.
	lit := findCompositeLit(decl, typeName)
	if lit == nil {
		return nil, diagnostic(fmt.Sprintf(
			"astedit: %s does not return a %s{…} literal, so there is nowhere to assign %s",
			fn, typeName, name))
	}
	var out []byte
	closing := fset.Position(lit.Rbrace).Offset
	if len(lit.Elts) > 0 {
		last := lit.Elts[len(lit.Elts)-1]
		end, prefix := afterLast(src, fset.Position(last.End()).Offset)
		out, err = splice(src, end, prefix+" "+name+": "+name+",")
	} else {
		out, err = splice(src, closing, name+": "+name)
	}
	if err != nil {
		return nil, err
	}

	// Re-parse: the splice above invalidated every offset.
	fset = token.NewFileSet()
	f, err = parser.ParseFile(fset, "controller.go", out, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("astedit: %w", err)
	}
	decl = findFunc(f, fn)
	if decl == nil {
		return nil, diagnostic("astedit: the constructor vanished after the literal edit")
	}
	params := decl.Type.Params
	if len(params.List) == 0 {
		return splice(out, fset.Position(params.Opening).Offset+1, name+" "+typ)
	}
	last := params.List[len(params.List)-1]
	end, prefix := afterLast(out, fset.Position(last.End()).Offset)
	return splice(out, end, prefix+"\n"+name+" "+typ+",")
}

// AddStatement appends stmt as the last statement of the named function or
// method body.
func AddStatement(src []byte, fn, stmt string) ([]byte, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "controller.go", src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("astedit: %w", err)
	}
	decl := findFunc(f, fn)
	if decl == nil || decl.Body == nil {
		return nil, diagnostic(fmt.Sprintf(
			"astedit: no function %s with a body in this file — Register is where a route is declared", fn))
	}
	// Textual, not structural: the statement is code the caller wrote, and
	// comparing rendered ASTs to decide "already there" would be a worse
	// test than comparing the text that was asked for.
	if strings.Contains(string(src), stmt) {
		return src, nil
	}
	// A ROUTE is identified by the handler it serves, not by its path. Once
	// `warren g command --route` existed, re-running the generator without
	// the flag re-derived a different path for the same handler and appended
	// it — leaving a second, un-asked-for public endpoint that nobody
	// noticed:
	//
	//	transport.Post(r, "/copies/{id}/checkout", c.checkoutCopy)
	//	transport.Post(r, "/checkout_copy", c.checkoutCopy)      ← new
	//
	// The path is the user's to choose and may well have been edited by
	// hand; the handler reference is what says this registration already
	// exists.
	if ref, ok := handlerRef(stmt); ok && strings.Contains(string(src), ref) {
		return src, nil
	}

	closing := fset.Position(decl.Body.Rbrace).Offset
	indent := indentOf(src, closing) + "\t"
	if n := len(decl.Body.List); n > 0 {
		indent = indentOf(src, fset.Position(decl.Body.List[n-1].Pos()).Offset)
	}
	return splice(src, lineStart(src, closing), indent+stmt+"\n")
}

// handlerRef extracts the handler argument a route statement ends with —
// "c.checkoutCopy)" from `transport.Post(r, "/x", c.checkoutCopy)`. It is
// the part of the statement that identifies WHICH registration this is.
func handlerRef(stmt string) (string, bool) {
	close := strings.LastIndex(stmt, ")")
	if close < 0 {
		return "", false
	}
	comma := strings.LastIndex(stmt[:close], ",")
	if comma < 0 {
		return "", false
	}
	ref := strings.TrimSpace(stmt[comma+1 : close])
	if ref == "" || !strings.HasPrefix(ref, "c.") {
		return "", false
	}
	return ref + ")", true
}

func findStruct(f *ast.File, name string) (*ast.StructType, error) {
	var found *ast.StructType
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != name {
			return true
		}
		if st, ok := ts.Type.(*ast.StructType); ok {
			found = st
		}
		return false
	})
	if found == nil {
		return nil, diagnostic(fmt.Sprintf(
			"astedit: no struct type %s in this file — a controller holds its handlers as fields", name))
	}
	return found, nil
}

func hasField(st *ast.StructType, name string) bool {
	for _, f := range st.Fields.List {
		for _, id := range f.Names {
			if id.Name == name {
				return true
			}
		}
	}
	return false
}

func findFunc(f *ast.File, name string) *ast.FuncDecl {
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == name {
			return fd
		}
	}
	return nil
}

func hasParam(fd *ast.FuncDecl, name string) bool {
	for _, p := range fd.Type.Params.List {
		for _, id := range p.Names {
			if id.Name == name {
				return true
			}
		}
	}
	return false
}

// findCompositeLit finds the <typeName>{…} the function builds, whether it
// is returned by value or by pointer.
func findCompositeLit(fd *ast.FuncDecl, typeName string) *ast.CompositeLit {
	var found *ast.CompositeLit
	ast.Inspect(fd, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || found != nil {
			return true
		}
		if id, ok := lit.Type.(*ast.Ident); ok && id.Name == typeName {
			found = lit
		}
		return true
	})
	return found
}

// lineStart returns the offset of the beginning of the line offset sits on,
// so an insertion lands above it rather than beside it.
func lineStart(src []byte, offset int) int {
	for i := offset - 1; i >= 0; i-- {
		if src[i] == '\n' {
			return i + 1
		}
	}
	return 0
}
