// Package application is a fixture: it carries a type whose name collides with
// two others, in packages whose LAST path element is also "application".
//
// The name is not incidental. `warren g module` names every feature's use-case
// package "application", so every feature in every Warren service shares that
// namespace by construction — which is why the collision this fixture
// reproduces is certain rather than possible.
package application

// BookView is catalog's view of a book. It shares a name and a package name
// with lending's, and has no field in common with it.
type BookView struct {
	ID              string `json:"id"`
	ISBN            string `json:"isbn"`
	CopiesOwned     int    `json:"copies_owned"`
	CopiesAvailable int    `json:"copies_available"`
}
