// Package domain is the contracts side of the §1.2 fixture graph the di
// tests run against. Its name is load-bearing: the golden diagnostic prints
// "domain.UserRepository".
package domain

// UserRepository is the port the fixture handler requires.
type UserRepository interface {
	FindByID(id string) (string, error)
}
