// The §10 use case, compiled as a test: Handle's body is warren.md's
// verbatim. The repository and unit-of-work interfaces around it are the
// user-side declarations §10 assumes — persistence.UnitOfWork is not yet
// specified, so the uow field is the user's own port here.
package app_test

import (
	"context"
	"testing"

	"github.com/MerseniBilel/warren/app"
	domain "github.com/MerseniBilel/warren/app/internal/exampledomain"
	"github.com/MerseniBilel/warren/errors"
)

type (
	UserDTO struct {
		Email string
		Name  string
	}

	RegisterUser struct {
		Email string
		Name  string
	}
)

type userRepo interface {
	ExistsByEmail(ctx context.Context, email domain.Email) (bool, error)
	Save(ctx context.Context, u *domain.User) error
}

type unitOfWork interface {
	Do(ctx context.Context, fn func(context.Context) error) error
}

type RegisterUserHandler struct {
	users userRepo
	uow   unitOfWork
}

func toDTO(u *domain.User) UserDTO {
	return UserDTO{Email: u.Email.String(), Name: u.Name}
}

func (h *RegisterUserHandler) Handle(ctx context.Context, cmd RegisterUser) (UserDTO, error) {
	email, err := domain.NewEmail(cmd.Email)
	if err != nil {
		return UserDTO{}, errors.Invalid("email", err)
	}
	if taken, _ := h.users.ExistsByEmail(ctx, email); taken {
		return UserDTO{}, errors.Conflict("user already exists")
	}

	u := domain.NewUser(email, cmd.Name)

	// Aggregate state + outbox row commit in ONE transaction.
	if err := h.uow.Do(ctx, func(ctx context.Context) error {
		return h.users.Save(ctx, u)
	}); err != nil {
		return UserDTO{}, err
	}
	return toDTO(u), nil
}

// No net/http. No pgx. No kgo. That is the entire point.

var _ app.Handler[RegisterUser, UserDTO] = (*RegisterUserHandler)(nil)

type fakeRepo struct {
	taken bool
	saved *domain.User
}

func (f *fakeRepo) ExistsByEmail(context.Context, domain.Email) (bool, error) {
	return f.taken, nil
}

func (f *fakeRepo) Save(_ context.Context, u *domain.User) error {
	f.saved = u
	return nil
}

type fakeUoW struct{ calls int }

func (f *fakeUoW) Do(ctx context.Context, fn func(context.Context) error) error {
	f.calls++
	return fn(ctx)
}

func TestSection10Handler(t *testing.T) {
	t.Parallel()

	t.Run("registers through the chain", func(t *testing.T) {
		t.Parallel()
		repo, uow := &fakeRepo{}, &fakeUoW{}
		var trace []string
		h := app.Chain(
			app.Handler[RegisterUser, UserDTO](&RegisterUserHandler{users: repo, uow: uow}),
			func(next app.Handler[RegisterUser, UserDTO]) app.Handler[RegisterUser, UserDTO] {
				return app.HandlerFunc[RegisterUser, UserDTO](func(ctx context.Context, cmd RegisterUser) (UserDTO, error) {
					trace = append(trace, "core middleware")
					return next.Handle(ctx, cmd)
				})
			},
		)

		dto, err := h.Handle(context.Background(), RegisterUser{Email: "bob@example.com", Name: "Bob"})
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if dto.Email != "bob@example.com" || uow.calls != 1 || repo.saved == nil || len(trace) != 1 {
			t.Errorf("dto=%+v uow.calls=%d saved=%v trace=%v", dto, uow.calls, repo.saved, trace)
		}
	})

	t.Run("duplicate email is a conflict", func(t *testing.T) {
		t.Parallel()
		h := &RegisterUserHandler{users: &fakeRepo{taken: true}, uow: &fakeUoW{}}
		_, err := h.Handle(context.Background(), RegisterUser{Email: "bob@example.com"})
		if !errors.Is(err, errors.CodeConflict) {
			t.Errorf("Handle() = %v, want a CONFLICT error", err)
		}
	})

	t.Run("bad email is invalid", func(t *testing.T) {
		t.Parallel()
		h := &RegisterUserHandler{users: &fakeRepo{}, uow: &fakeUoW{}}
		_, err := h.Handle(context.Background(), RegisterUser{Email: "not-an-address"})
		if !errors.Is(err, errors.CodeInvalid) {
			t.Errorf("Handle() = %v, want an INVALID error", err)
		}
	})
}
