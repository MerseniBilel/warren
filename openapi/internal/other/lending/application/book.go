// Package application is the third of the collision fixture, and the one that
// forces a THIRD level of qualification: its parent is also "lending", so it
// is still ambiguous with fixture/lending/application at two segments.
package application

// BookView is a third same-named view, under a different grandparent.
type BookView struct {
	Archived bool `json:"archived"`
}
