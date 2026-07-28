module github.com/MerseniBilel/warren

// Warren tracks the current Go major release, with no toolchain directive and
// no compatibility path to older releases. See AGENT.md § Non-negotiable
// invariants.
//
// The core module carries no third-party dependencies by design: every heavy
// dependency lives in its own submodule so that a minimal Warren service has a
// go.mod an auditor can read in one screen. See docs/architecture.md § 2.
go 1.26
