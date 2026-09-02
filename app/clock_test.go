package app_test

import (
	"testing"
	"time"

	"github.com/MerseniBilel/warren/app"
)

// TestClockNeedsNoFakeType is the point of the function type: substituting a
// clock in a test is a closure, not a struct implementing an interface, and
// not a mocking library — which AGENT.md forbids in this repository anyway.
func TestClockNeedsNoFakeType(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	var c app.Clock = func() time.Time { return at }

	if got := c(); !got.Equal(at) {
		t.Errorf("c() = %v, want %v", got, at)
	}
	if got := c.Now(); !got.Equal(at) {
		t.Errorf("c.Now() = %v, want %v — the method and the call must agree", got, at)
	}
}

func TestSystemClockIsJustTimeNow(t *testing.T) {
	t.Parallel()

	var c app.Clock = time.Now
	before := time.Now()
	got := c.Now()
	if got.Before(before) {
		t.Errorf("Clock(time.Now) returned %v, before %v", got, before)
	}
}
