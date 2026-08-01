// The §3.1 usage example, compiled as a test: the shape every generated
// aggregate follows. NewUser and Activate are verbatim from warren.md; the
// identifier, email, status, and event types around them are the user-side
// definitions §3.1 assumes.
package domain_test

import (
	"testing"
	"time"

	"github.com/MerseniBilel/warren/domain"
	"github.com/MerseniBilel/warren/errors"
)

type UserID string

func (id UserID) String() string { return string(id) }

// NewUserID mints the identifier before the first event is raised. A real
// application generates one; the strategy is the user's, not Warren's.
func NewUserID() UserID { return "user-1" }

type Email string

func (e Email) String() string { return string(e) }

type Status int

const (
	StatusPending Status = iota
	StatusActive
)

type UserRegistered struct {
	UserID UserID
	Email  string
	At     time.Time
}

func (e UserRegistered) EventName() string     { return "user.registered" }
func (e UserRegistered) OccurredAt() time.Time { return e.At }
func (e UserRegistered) AggregateID() string   { return e.UserID.String() }

type UserActivated struct {
	UserID UserID
	At     time.Time
}

func (e UserActivated) EventName() string     { return "user.activated" }
func (e UserActivated) OccurredAt() time.Time { return e.At }
func (e UserActivated) AggregateID() string   { return e.UserID.String() }

type User struct {
	domain.AggregateRoot[UserID]
	Email  Email
	Name   string
	Status Status
}

func NewUser(email Email, name string) *User {
	u := &User{
		AggregateRoot: domain.NewAggregateRoot(NewUserID()),
		Email:         email,
		Name:          name,
		Status:        StatusPending,
	}
	u.Raise(UserRegistered{UserID: u.ID(), Email: email.String(), At: time.Now()})
	return u
}

func (u *User) Activate() error {
	if u.Status == StatusActive {
		return errors.Conflict("user already active")
	}
	u.Status = StatusActive
	u.Raise(UserActivated{UserID: u.ID(), At: time.Now()})
	return nil
}

var _ domain.Root[UserID] = (*User)(nil)

func TestSection31Example(t *testing.T) {
	t.Parallel()

	u := NewUser("bob@example.com", "Bob")
	events := u.PullEvents()
	if len(events) != 1 || events[0].EventName() != "user.registered" {
		t.Fatalf("NewUser raised %v, want one user.registered event", events)
	}

	if err := u.Activate(); err != nil {
		t.Fatalf("first Activate() = %v, want nil", err)
	}
	if events := u.PullEvents(); len(events) != 1 || events[0].EventName() != "user.activated" {
		t.Fatalf("Activate raised %v, want one user.activated event", events)
	}

	err := u.Activate()
	if !errors.Is(err, errors.CodeConflict) {
		t.Errorf("second Activate() = %v, want a CONFLICT error", err)
	}
}
