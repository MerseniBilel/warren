package playground

import (
	"fmt"
	"reflect"
	"strings"
)

// diagnostic carries a rendered multi-line block; the text is the contract,
// covered by golden files like every other Warren diagnostic.
type diagnostic string

func (d diagnostic) Error() string { return string(d) }

func errNotAStruct(t reflect.Type) error {
	return diagnostic(fmt.Sprintf(
		"✗ cannot validate %s\n\n"+
			"    A request type must be a struct: constraints live on fields, and a\n"+
			"    %s has none.\n\n"+
			"  Wrap it — type Req struct { Value %s `validate:\"required\"` } — or\n"+
			"  turn validation off for this application with validate.None().",
		t, t.Kind(), t))
}

// errUnknownConstraint is the reason Plan walks the type at boot at all.
//
// go-playground PANICS on an unrecognised tag, and it panics at validation
// time — so a typo in a field nobody has exercised yet takes the process down
// on the first request that touches it, in production, with a stack trace.
// Catching it here makes it a boot failure naming the field and the typo.
func errUnknownConstraint(t reflect.Type, field string, tokens []string) error {
	return diagnostic(fmt.Sprintf(
		"✗ unknown validation constraint\n\n"+
			"    %s field %q uses constraints go-playground does not know:\n"+
			"    %s\n\n"+
			"  This is almost always a typo — \"emai\" for \"email\", \"uuid5\" for\n"+
			"  \"uuid4\". The library panics on an unknown tag when it VALIDATES, so\n"+
			"  catching it here is the difference between a boot failure and a\n"+
			"  production crash on the first request that touches this field.\n\n"+
			"  Register your own with:\n\n"+
			"      playground.New(playground.Register(%q, func(v any) bool { ... }))",
		t, field, strings.Join(tokens, ", "), tokens[0]))
}
