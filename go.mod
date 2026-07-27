module github.com/MerseniBilel/warren

// Warren tracks the current Go major release. See docs/adr/0007-go-version-policy.md.
// The core module carries no third-party dependencies by design: every heavy
// dependency lives in its own submodule so that a minimal Warren service has a
// go.mod an auditor can read in one screen. See docs/adr/0003-repo-layout.md.
go 1.26
