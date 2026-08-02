package generate

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Names reaching a generator become directory paths, package names, import
// identifiers and type names. This file is the gate they all pass through.
//
// The rule is that a generator may only ever write inside the project it
// was pointed at, and may only ever produce Go that compiles. A name that
// cannot do both is refused before a single byte is written — because the
// alternative is a tool that corrupts source it did not author, in a
// project the user did not name.

// goKeywords cannot be a package name or a type name.
var goKeywords = []string{
	"break", "case", "chan", "const", "continue", "default", "defer", "else",
	"fallthrough", "for", "func", "go", "goto", "if", "import", "interface",
	"map", "package", "range", "return", "select", "struct", "switch", "type", "var",
}

// goPredeclared are legal identifiers, which is exactly what makes them
// dangerous: a package named `nil` shadows the real `nil` for every file
// that imports it, so the scaffold's own `if err != nil` stops compiling —
// code the generator never touched.
var goPredeclared = []string{
	"any", "bool", "byte", "comparable", "complex64", "complex128", "error",
	"float32", "float64", "int", "int8", "int16", "int32", "int64", "rune",
	"string", "uint", "uint8", "uint16", "uint32", "uint64", "uintptr",
	"true", "false", "iota", "nil",
	"append", "cap", "clear", "close", "complex", "copy", "delete", "imag",
	"len", "make", "max", "min", "new", "panic", "print", "println", "real", "recover",
}

// checkModuleName validates the name of a feature module, which becomes a
// directory, a package name and an import identifier all at once.
func checkModuleName(name string) error {
	if name == "" {
		return errMissingName("a module name")
	}
	if err := checkNoPath(name, "module name"); err != nil {
		return err
	}
	lower := strings.ToLower(name)
	if !isIdentifier(lower) {
		return errNotAnIdentifier(name, "a module name becomes a package name and an import identifier")
	}
	if slices.Contains(goKeywords, lower) {
		return errReservedName(name, "a Go keyword")
	}
	if slices.Contains(goPredeclared, lower) {
		return errShadowsPredeclared(lower)
	}
	return nil
}

// checkTypeName validates the name of a generated aggregate, command or
// event. It must be EXPORTED: the domain declares the port, infrastructure
// implements it from another package, and an unexported name makes that
// impossible.
func checkTypeName(name string) error {
	if name == "" {
		return errMissingName("a name")
	}
	if err := checkNoPath(name, "name"); err != nil {
		return err
	}
	if !isIdentifier(name) {
		return errNotAnIdentifier(name, "a name becomes a Go type name")
	}
	if slices.Contains(goKeywords, name) {
		return errReservedName(name, "a Go keyword")
	}
	if first, _ := utf8.DecodeRuneInString(name); !unicode.IsUpper(first) {
		return diagnostic(fmt.Sprintf(
			"✗ %q is not exported\n\n    Generated types have to be exported: the domain declares the port\n"+
				"    and infrastructure implements it from another package.\n\n  Try:  %s",
			name, upperFirst(name)))
	}
	return nil
}

// checkNoPath refuses anything that would make a name traverse the file
// system. filepath.Join cleans "..", so an unchecked segment escapes the
// project silently and rewrites another one's module.go.
func checkNoPath(name, what string) error {
	if !strings.ContainsAny(name, `/\`) && name != ".." && name != "." {
		return nil
	}
	return diagnostic(fmt.Sprintf(
		"✗ %q is not a valid %s\n\n    A %s is a single identifier, not a path.\n\n"+
			"  `warren g` only ever writes inside the project given by --dir.",
		name, what, what))
}

// isIdentifier reports whether s is a legal Go identifier. It decodes
// runes rather than bytes: Go identifiers may be non-ASCII, and slicing a
// multi-byte name produces invalid UTF-8 in the output.
func isIdentifier(s string) bool {
	if s == "" || s == "_" {
		return false
	}
	for i, r := range s {
		switch {
		case unicode.IsLetter(r) || r == '_':
		case unicode.IsDigit(r) && i > 0:
		default:
			return false
		}
	}
	return true
}

// checkNotDeclared refuses a generator that would redeclare an identifier
// the package already has.
//
// The conflict gate compares FILE PATHS, so it cannot see this: `g command
// Ship` writes ship.go and `g consumer Ship` writes on_ship.go, and both
// declare `type Ship`. Different paths, same package, and the package stops
// compiling.
func checkNotDeclared(dir, pkgDir string, skip map[string][]byte, names ...string) error {
	full := filepath.Join(dir, pkgDir)
	entries, err := os.ReadDir(full)
	if err != nil {
		return nil // no package yet, so nothing to collide with
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		// A generator's own previous output is not a redeclaration; it is a
		// path conflict, which the conflict gate and --force already own.
		if _, own := skip[filepath.ToSlash(filepath.Join(pkgDir, e.Name()))]; own {
			continue
		}
		src, rerr := os.ReadFile(filepath.Join(full, e.Name()))
		if rerr != nil {
			continue
		}
		f, perr := parser.ParseFile(fset, e.Name(), src, parser.SkipObjectResolution)
		if perr != nil {
			continue // a file that does not parse cannot be reasoned about
		}
		if found, where := declares(f, names); found != "" {
			return errRedeclared(found, filepath.ToSlash(filepath.Join(pkgDir, e.Name())), where)
		}
	}
	return nil
}

// declares reports the first of names the file declares at package level,
// and what kind of declaration it is.
func declares(f *ast.File, names []string) (string, string) {
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil && slices.Contains(names, d.Name.Name) {
				return d.Name.Name, "a function"
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if slices.Contains(names, s.Name.Name) {
						return s.Name.Name, "a type"
					}
				case *ast.ValueSpec:
					for _, id := range s.Names {
						if slices.Contains(names, id.Name) {
							return id.Name, "a variable or constant"
						}
					}
				}
			}
		}
	}
	return "", ""
}

func errNotAnIdentifier(name, why string) error {
	return diagnostic(fmt.Sprintf(
		"✗ %q is not a valid Go identifier\n\n    %s, so it has to start with a letter\n"+
			"    and contain only letters and digits.", name, upperFirst(why)))
}

func errReservedName(name, what string) error {
	return diagnostic(fmt.Sprintf("✗ %q is %s\n\n    Pick a name that is not reserved.", name, what))
}

func errShadowsPredeclared(name string) error {
	return diagnostic(fmt.Sprintf(
		"✗ %q shadows a predeclared identifier\n\n"+
			"    A package named %q would shadow the real %s for every file that\n"+
			"    imports it — including the scaffold's own code, which would stop\n"+
			"    compiling for a reason nothing points at.\n\n  Pick another name.",
		name, name, name))
}

func errRedeclared(name, file, kind string) error {
	return diagnostic(fmt.Sprintf(
		"✗ %s is already declared\n\n    %s declares %s as %s.\n\n"+
			"    The generated file would sit in the same package, so the package\n"+
			"    would stop compiling — and the conflict check cannot see it,\n"+
			"    because the two files have different names.\n\n"+
			"  Pick another name.", name, file, name, kind))
}

// upperFirst is rune-safe: slicing a byte off a multi-byte name produces
// invalid UTF-8, which then fails inside a template and blames the CLI for
// the user's input.
func upperFirst(s string) string {
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[size:]
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	return string(unicode.ToLower(r)) + s[size:]
}
