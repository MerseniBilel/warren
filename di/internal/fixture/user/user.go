// Package user is the consuming module of the §1.2 fixture graph. Its name
// is load-bearing: the golden diagnostic prints "*user.RegisterUserHandler"
// and "*user.UserController".
package user

import "github.com/MerseniBilel/warren/di/internal/fixture/domain"

// RegisterUserHandler requires the repository — the first link of the golden
// diagnostic's chain.
type RegisterUserHandler struct{ repo domain.UserRepository }

// NewRegisterUserHandler constructs the handler.
func NewRegisterUserHandler(repo domain.UserRepository) *RegisterUserHandler {
	return &RegisterUserHandler{repo: repo}
}

// UserController requires the handler — the second link of the chain.
//
//nolint:revive // the stuttering name is load-bearing: the §2.2 golden diagnostic prints "*user.UserController"
type UserController struct{ handler *RegisterUserHandler }

// NewUserController constructs the controller.
func NewUserController(handler *RegisterUserHandler) *UserController {
	return &UserController{handler: handler}
}

// UserService is private to the user module in the encapsulation suite:
// provided, never exported, and must be invisible to sibling scopes.
//
//nolint:revive // the stuttering name is §1.2's: the diagnostic must say a *user.UserService stayed private
type UserService struct{}

// NewUserService constructs the service.
func NewUserService() *UserService { return &UserService{} }
