package validate_test

import (
	stderrors "errors"
	"reflect"
	"strings"
	"testing"

	werrors "github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/validate"
)

type registerUser struct {
	Email string `json:"email" validate:"required"`
	Name  string `json:"name"  validate:"required"`
	Note  string `json:"note"`
}

type nested struct {
	Address struct {
		Postcode string `json:"postcode" validate:"required"`
	} `json:"address"`
}

func TestRequiredPlan(t *testing.T) {
	t.Parallel()

	v := validate.Required()

	t.Run("a valid value passes", func(t *testing.T) {
		t.Parallel()
		rule, err := validate.PlanFor[registerUser](v)
		if err != nil {
			t.Fatalf("PlanFor: %v", err)
		}
		if err := rule(&registerUser{Email: "bob@example.com", Name: "Bob"}); err != nil {
			t.Errorf("rule = %v, want nil", err)
		}
	})

	t.Run("one missing field names it with its json key", func(t *testing.T) {
		t.Parallel()
		rule, _ := validate.PlanFor[registerUser](v)
		err := rule(&registerUser{Name: "Bob"})
		if !werrors.Is(err, werrors.CodeInvalid) {
			t.Fatalf("err = %v, want CodeInvalid", err)
		}
		if !strings.Contains(err.Error(), "email") {
			t.Errorf("err = %v, want the field named", err)
		}
		var we *werrors.Error
		if !as(err, &we) {
			t.Fatal("not a *errors.Error")
		}
		if we.Details()["email"] == nil {
			t.Errorf("details = %v, want an entry keyed by the json name", we.Details())
		}
	})

	t.Run("every missing field is reported at once", func(t *testing.T) {
		t.Parallel()
		rule, _ := validate.PlanFor[registerUser](v)
		err := rule(&registerUser{})
		var we *werrors.Error
		if !as(err, &we) {
			t.Fatalf("err = %v", err)
		}
		d := we.Details()
		if d["email"] == nil || d["name"] == nil {
			t.Errorf("details = %v, want both fields — one request, every failure", d)
		}
	})

	t.Run("nested fields are dotted", func(t *testing.T) {
		t.Parallel()
		rule, err := validate.PlanFor[nested](v)
		if err != nil {
			t.Fatalf("PlanFor: %v", err)
		}
		verr := rule(&nested{})
		var we *werrors.Error
		if !as(verr, &we) {
			t.Fatalf("err = %v", verr)
		}
		if we.Details()["address.postcode"] == nil {
			t.Errorf("details = %v, want address.postcode", we.Details())
		}
	})

	t.Run("an untagged struct plans to a no-op", func(t *testing.T) {
		t.Parallel()
		type plain struct{ A string }
		rule, err := validate.PlanFor[plain](v)
		if err != nil {
			t.Fatalf("PlanFor: %v", err)
		}
		if err := rule(&plain{}); err != nil {
			t.Errorf("rule = %v, want nil", err)
		}
	})
}

func TestRequiredRejectsRicherVocabularyAtPlanTime(t *testing.T) {
	t.Parallel()

	type richer struct {
		Email string `json:"email" validate:"required,email"`
		Age   int    `json:"age" validate:"gte=18"`
	}
	_, err := validate.PlanFor[richer](validate.Required())
	if err == nil {
		t.Fatal("Plan accepted tags it cannot enforce — silently under-validating is the failure mode this refuses")
	}
	for _, want := range []string{"email", "gte=18", "validate/playground"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q — it must name the tokens and the fix:\n%s", want, err)
		}
	}
}

func TestEmptinessByKind(t *testing.T) {
	t.Parallel()

	type kinds struct {
		S     string         `validate:"required"`
		N     int            `validate:"required"`
		Slice []string       `validate:"required"`
		M     map[string]int `validate:"required"`
		P     *string        `validate:"required"`
		B     bool           `validate:"required"`
	}
	rule, err := validate.PlanFor[kinds](validate.Required())
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	verr := rule(&kinds{})
	var we *werrors.Error
	if !as(verr, &we) {
		t.Fatalf("err = %v", verr)
	}
	for _, f := range []string{"S", "N", "Slice", "M", "P", "B"} {
		if we.Details()[f] == nil {
			t.Errorf("zero %s was accepted as present", f)
		}
	}

	s := "x"
	full := &kinds{S: "x", N: 1, Slice: []string{"a"}, M: map[string]int{"a": 1}, P: &s, B: true}
	if err := rule(full); err != nil {
		t.Errorf("a fully populated value failed: %v", err)
	}
}

func TestPlanIsTypeChecked(t *testing.T) {
	t.Parallel()

	_, err := validate.Required().Plan(reflect.TypeFor[int]())
	if err == nil {
		t.Error("Plan accepted a non-struct type")
	}
}

func as(err error, target **werrors.Error) bool {
	e, ok := err.(*werrors.Error)
	if ok {
		*target = e
	}
	return ok
}

func BenchmarkRulePass(b *testing.B) {
	rule, _ := validate.PlanFor[registerUser](validate.Required())
	v := &registerUser{Email: "bob@example.com", Name: "Bob"}
	b.ReportAllocs()
	for b.Loop() {
		_ = rule(v)
	}
}

func BenchmarkRuleFail(b *testing.B) {
	rule, _ := validate.PlanFor[registerUser](validate.Required())
	v := &registerUser{}
	b.ReportAllocs()
	for b.Loop() {
		_ = rule(v)
	}
}

// TestNoneIsTheExplicitOptOut pins the escape hatch the 2026-08-02 review
// found missing: before validate/playground ships, a project using a common
// tag like validate:"email" could not boot at all.
func TestNoneIsTheExplicitOptOut(t *testing.T) {
	t.Parallel()

	type richer struct {
		Email string `json:"email" validate:"required,email"`
	}
	rule, err := validate.PlanFor[richer](validate.None())
	if err != nil {
		t.Fatalf("None() refused a plan: %v", err)
	}
	if err := rule(&richer{}); err != nil {
		t.Errorf("None() enforced something: %v — opting out means opting out", err)
	}

	// And the refusal names both ways forward.
	_, err = validate.PlanFor[richer](validate.Required())
	if err == nil {
		t.Fatal("Required() accepted a tag it cannot enforce")
	}
	for _, want := range []string{"validate/playground", "validate.None()"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the diagnostic does not offer %q:\n%s", want, err)
		}
	}
}

// TestEmbeddedFieldsFlattenLikeJSON — encoding/json PROMOTES the exported
// fields of an embedded struct, exported or not, so a required field on one
// is decoded from the top level. Skipping unexported embeds under-validated
// silently; keying an exported embed as "Base.email" named a path no client
// ever sends.
func TestEmbeddedFieldsFlattenLikeJSON(t *testing.T) {
	t.Parallel()

	type base struct {
		Email string `json:"email" validate:"required"`
	}
	type Exported struct {
		Phone string `json:"phone" validate:"required"`
	}
	type req struct {
		base
		Exported
		Name string `json:"name" validate:"required"`
	}

	rule, err := validate.PlanFor[req](validate.Required())
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	err = rule(&req{})
	if err == nil {
		t.Fatal("an empty request passed validation")
	}
	var e *werrors.Error
	if !stderrors.As(err, &e) {
		t.Fatalf("err = %v", err)
	}
	got := e.Details()
	for _, want := range []string{"email", "phone", "name"} {
		if _, ok := got[want]; !ok {
			t.Errorf("detail key %q is missing — got %v", want, got)
		}
	}
	for bad := range got {
		if strings.Contains(bad, ".") {
			t.Errorf("embedded field keyed as %q; encoding/json promotes it to the top level", bad)
		}
	}
}

// TestPointerNestedStructsAreDescended — a *Address marked required was
// checked for non-nil and its own required fields were never looked at.
func TestPointerNestedStructsAreDescended(t *testing.T) {
	t.Parallel()

	type address struct {
		Postcode string `json:"postcode" validate:"required"`
	}
	type req struct {
		Address *address `json:"address" validate:"required"`
	}

	rule, err := validate.PlanFor[req](validate.Required())
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	// Present but empty inside: the nil check passes, the contents must not.
	err = rule(&req{Address: &address{}})
	if err == nil {
		t.Fatal("a present-but-empty nested struct passed validation")
	}
	var e *werrors.Error
	if !stderrors.As(err, &e) {
		t.Fatalf("err = %v", err)
	}
	if _, ok := e.Details()["address.postcode"]; !ok {
		t.Errorf("details = %v, want a key for address.postcode", e.Details())
	}
	// A nil pointer is still caught by required, and does not panic.
	if err := rule(&req{}); err == nil {
		t.Error("a nil required pointer passed validation")
	}
}

// TestElementValidationIsRefusedNotSkipped — a []LineItem whose element
// type carries validate tags was passed over in silence. This package's own
// doc comment sells refusal over silent under-validation; it has to hold
// for the case it cannot yet handle.
func TestElementValidationIsRefusedNotSkipped(t *testing.T) {
	t.Parallel()

	type item struct {
		SKU string `json:"sku" validate:"required"`
	}
	type req struct {
		Items []item `json:"items" validate:"required"`
	}

	_, err := validate.PlanFor[req](validate.Required())
	if err == nil {
		t.Fatal("a slice of tagged elements planned silently — its elements are never validated")
	}
	for _, want := range []string{"items", "None()"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the diagnostic is missing %q:\n%v", want, err)
		}
	}
}
