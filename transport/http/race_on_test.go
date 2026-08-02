//go:build race

package http

// raceEnabled tells the allocation test to stand down: the race detector adds
// bookkeeping allocations, so a fixed budget measured without it cannot hold.
const raceEnabled = true
