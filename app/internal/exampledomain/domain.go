// Package domain is the user-side domain of the §10 example, so the app
// tests can compile warren.md's handler verbatim. Its package name is
// load-bearing: the handler body says domain.NewEmail and domain.NewUser.
package domain

import (
	"strings"

	werrors "github.com/MerseniBilel/warren/errors"
)

// Email is the example's validated value object.
type Email string

// String renders the address.
func (e Email) String() string { return string(e) }

// NewEmail validates and returns the address.
func NewEmail(s string) (Email, error) {
	if !strings.Contains(s, "@") {
		return "", werrors.Invalid("email", nil)
	}
	return Email(s), nil
}

// User is the example aggregate, reduced to what the handler touches.
type User struct {
	Email Email
	Name  string
}

// NewUser constructs the aggregate.
func NewUser(email Email, name string) *User {
	return &User{Email: email, Name: name}
}
