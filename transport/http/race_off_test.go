//go:build !race

package http

// raceEnabled is false in the ordinary build, where the allocation budget is
// meaningful. See race_on_test.go.
const raceEnabled = false
