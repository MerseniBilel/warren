// Package postgres is the providing module of the §1.2 fixture graph. Its
// name is load-bearing: the golden diagnostic prints
// "postgres.NewUserRepository". It contains no driver — it stands in for the
// adapter that would.
package postgres

import "github.com/MerseniBilel/warren/di/internal/fixture/domain"

type userRepository struct{}

func (userRepository) FindByID(string) (string, error) { return "", nil }

// NewUserRepository constructs the repository implementation.
func NewUserRepository() domain.UserRepository { return userRepository{} }
