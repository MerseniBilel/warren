package lifecycle

import "strconv"

// State is a point in the lifecycle. Its zero value is StateNew, so a Hooks
// that has not started reads as not ready.
type State uint8

const (
	// StateNew reports that the lifecycle was constructed and never started.
	StateNew State = iota
	// StateStarting reports that OnStart hooks are running.
	StateStarting
	// StateReady reports that every OnStart succeeded. It is the only state
	// that means ready.
	StateReady
	// StateStopping reports that OnStop hooks are running.
	StateStopping
	// StateStopped reports that the lifecycle has fully stopped. It is
	// terminal: a process that wants to come back up is a new process.
	StateStopped
)

// String returns the state's name, for example "Ready". A value outside the
// defined set renders as "State(7)", so a bad conversion is visible rather than
// blank.
func (s State) String() string {
	switch s {
	case StateNew:
		return "New"
	case StateStarting:
		return "Starting"
	case StateReady:
		return "Ready"
	case StateStopping:
		return "Stopping"
	case StateStopped:
		return "Stopped"
	default:
		return "State(" + strconv.Itoa(int(s)) + ")"
	}
}
