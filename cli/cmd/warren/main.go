// Command warren is Warren's CLI: it scaffolds applications, generates the
// code a feature needs, and enforces the architecture rules the framework
// is built on.
//
// It is the TOOLING ring: it may read a project to analyse it, and nothing
// in the runtime ever imports it.
package main

import (
	"fmt"
	"os"

	"github.com/MerseniBilel/warren/cli/internal/command"
)

func main() {
	if err := command.Root().Execute(); err != nil {
		// Warren's diagnostics are multi-line blocks; print them as written.
		if msg := err.Error(); msg != "" {
			fmt.Fprintln(os.Stderr, msg)
		}
		os.Exit(command.ExitCode(err))
	}
}
