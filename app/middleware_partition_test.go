package app

// This file is the one INTERNAL test in app: the partition it pins is between
// two unexported values, and asserting it from outside would mean copying one
// of them, which is the drift being prevented.

import (
	stderrors "errors"
	"slices"
	"testing"

	"github.com/MerseniBilel/warren/errors"
)

// sampleErrors is one error per code, so a predicate over codes can be
// exercised through the predicate's real signature.
func sampleErrors() map[errors.Code]error {
	return map[errors.Code]error{
		errors.CodeInvalid:          errors.Invalid("email", stderrors.New("bad")),
		errors.CodeNotFound:         errors.NotFound("user", 42),
		errors.CodeConflict:         errors.Conflict("already applied"),
		errors.CodeContention:       errors.Contention("the version moved"),
		errors.CodeUnauthenticated:  errors.Unauthenticated("no token"),
		errors.CodePermissionDenied: errors.PermissionDenied("delete the order"),
		errors.CodeUnavailable:      errors.Unavailable("postgres", stderrors.New("dial")),
		errors.CodeInternal:         errors.Internal(stderrors.New("boom")),
	}
}

// TestTerminalAndRetryableCodesPartitionTheVocabulary — `terminal` is written
// out by hand and `retryableCodes` is its complement, so the two halves are
// one partition of errors.Codes(). Nothing in the compiler says so: a ninth
// code, or a code moved between the halves, would leave RetryingOn's guard
// and RetryingOn's diagnostics describing different sets, which is exactly
// the class of defect this file exists to catch.
func TestTerminalAndRetryableCodesPartitionTheVocabulary(t *testing.T) {
	t.Parallel()

	all := errors.Codes()
	legal := retryableCodes()

	// Disjoint.
	for _, c := range legal {
		if slices.Contains(terminal, c) {
			t.Errorf("%s is both terminal and legal for RetryingOn", c)
		}
	}

	// Complete, and with no member of either half outside the vocabulary.
	union := append(slices.Clone(terminal), legal...)
	for _, c := range all {
		if !slices.Contains(union, c) {
			t.Errorf("%s is neither terminal nor legal for RetryingOn — the partition has a hole", c)
		}
	}
	for _, c := range union {
		if !slices.Contains(all, c) {
			t.Errorf("%s is in the partition but not in errors.Codes()", c)
		}
	}
	if len(union) != len(all) {
		t.Errorf("the partition has %d codes, errors.Codes() has %d", len(union), len(all))
	}
}

// TestRetryableCodesAreTheSetTheDiagnosticsName — the panic messages promise
// a specific three codes. If the partition moves, this is the assertion that
// makes the promise fail rather than quietly become false.
func TestRetryableCodesAreTheSetTheDiagnosticsName(t *testing.T) {
	t.Parallel()

	want := []errors.Code{errors.CodeContention, errors.CodeUnavailable, errors.CodeInternal}
	if !slices.Equal(retryableCodes(), want) {
		t.Errorf("retryableCodes() = %v, want %v", retryableCodes(), want)
	}
	if got, wantList := retryableList(), "CONTENTION, UNAVAILABLE, INTERNAL"; got != wantList {
		t.Errorf("retryableList() = %q, want %q", got, wantList)
	}
}

// TestDefaultRetryableIsAStrictSubsetOfTheLegalSet — the difference between
// the two is CodeInternal, and that difference is the first of RetryingOn's
// two reasons to exist. If it ever became empty, RetryingOn would be a
// narrowing helper only, and its doc comment would be half wrong again.
func TestDefaultRetryableIsAStrictSubsetOfTheLegalSet(t *testing.T) {
	t.Parallel()

	legal := retryableCodes()
	samples := sampleErrors()

	var byDefault []errors.Code
	for _, c := range errors.Codes() {
		err, ok := samples[c]
		if !ok {
			t.Fatalf("no sample error for %s", c)
		}
		if !retryable(err) {
			continue
		}
		byDefault = append(byDefault, c)
		if !slices.Contains(legal, c) {
			t.Errorf("Retrying retries %s, which RetryingOn refuses — the two disagree", c)
		}
	}

	if want := []errors.Code{errors.CodeContention, errors.CodeUnavailable}; !slices.Equal(byDefault, want) {
		t.Errorf("Retrying retries %v, want %v", byDefault, want)
	}
	if retryable(samples[errors.CodeInternal]) {
		t.Error("Retrying retries INTERNAL; RetryingOn's first reason to exist is gone")
	}
	if !slices.Contains(legal, errors.CodeInternal) {
		t.Error("RetryingOn refuses INTERNAL; it is then reachable through no middleware at all")
	}
}
