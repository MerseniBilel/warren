// Package command builds the CLI's command tree.
package command

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/MerseniBilel/warren/cli/internal/arch"
	"github.com/MerseniBilel/warren/cli/internal/scaffold"
)

// Version is the CLI's version, stamped at build time. It is also the
// framework version a scaffold pins, so a CLI and the code it generates
// always agree.
var Version = "v0.1.0"

// Root returns the command tree.
func Root() *cobra.Command {
	root := &cobra.Command{
		Use:   "warren",
		Short: "The Warren CLI: scaffold, generate, and enforce architecture",
		Long: "warren scaffolds Warren applications, generates the code a feature\n" +
			"needs, and enforces the ring and layer rules the framework is built on.",
		SilenceErrors: true, // diagnostics are rendered by main, as written
		SilenceUsage:  true,
	}
	root.AddCommand(newCmd(), generateCmd(), lintCmd(), versionCmd())
	return root
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the CLI and framework versions",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "warren %s (framework %s)\n", Version, Version)
			return err
		},
	}
}

func lintCmd() *cobra.Command {
	lint := &cobra.Command{
		Use:   "lint",
		Short: "Enforce the rules the architecture rests on",
	}
	lint.AddCommand(lintArchCmd())
	return lint
}

func lintArchCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "arch [dir]",
		Short: "Check the layer and module rules against the import graph",
		Long: "arch reads the import graph and reports every package that breaks the\n" +
			"layer rule (domain imports nothing from the other three) or reaches\n" +
			"into another feature module's internals.\n\n" +
			"It reads imports syntactically, so it works on a project that does not\n" +
			"compile — which is when it matters most, because the fix for a layer\n" +
			"violation usually breaks the build first.\n\n" +
			"Exit codes: 0 clean · 1 violations found · 2 could not run.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				dir = args[0]
			}
			if dir == "" {
				dir = "."
			}
			report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
			if err != nil {
				// Exit 2: a CI that cannot tell "could not analyse" from
				// "clean" quietly stops enforcing anything.
				return &exitError{code: 2, err: err}
			}
			if _, werr := fmt.Fprint(cmd.OutOrStdout(), report.String()); werr != nil {
				return werr
			}
			if len(report.Violations) > 0 {
				return &exitError{code: 1}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "the module to check (default: the current directory)")
	return cmd
}

// exitError carries an exit code out of a command without printing twice.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return ""
}

// ExitCode reports the process exit code an error should produce.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	return 1
}

func newCmd() *cobra.Command {
	var opts scaffold.Options
	cmd := &cobra.Command{
		Use:   "new <name>",
		Short: "Scaffold a Warren application",
		Long: "new writes a working Warren service: a module graph, a feature with\n" +
			"its four layers, the transactional outbox, a consumer, and tests that\n" +
			"boot the whole thing.\n\n" +
			"Warren is not published yet, so `go build` in a fresh scaffold cannot\n" +
			"resolve the framework. Pass --framework <path-to-warren-checkout> and\n" +
			"the go.mod gets the replace directives that make it compile and pass\n" +
			"`go test` as generated. When v0.1.0 is tagged the flag stops being\n" +
			"necessary.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Name = args[0]
			opts.Version = Version
			if opts.Dir == "" {
				opts.Dir = args[0]
			}
			if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
				return err
			}
			if err := scaffold.New(opts); err != nil {
				return err
			}
			abs, _ := filepath.Abs(opts.Dir)
			_, err := fmt.Fprintf(cmd.OutOrStdout(),
				"Created %s\n\n%s  cd %s\n  go mod tidy\n  %s_NAME=%s go run ./cmd/%s\n  go test ./...\n\n"+
					"It serves POST /users, /healthz and /readyz on :8080.\n"+
					"README.md says what is there and what is not.\n",
				abs, unpublishedNotice(opts.FrameworkPath), opts.Dir,
				envPrefix(opts.Name), opts.Name, opts.Name)
			return err
		},
	}
	cmd.Flags().StringVar(&opts.ModulePath, "module", "", "the Go module path of the new app (required)")
	cmd.Flags().StringVar(&opts.Dir, "dir", "", "where to write it (default: the app's name)")
	cmd.Flags().StringVar(&opts.FrameworkPath, "framework", "", "path to a local Warren checkout, written as replace directives (needed until v0.1.0 is tagged)")
	cmd.Flags().StringVar(&opts.Transport, "transport", "", "transport adapter (none released yet)")
	cmd.Flags().StringVar(&opts.DB, "db", "memory", "persistence driver: memory")
	cmd.Flags().StringVar(&opts.Broker, "broker", "memory", "broker driver: memory")
	return cmd
}

// unpublishedNotice tells the user what --framework would have done, once,
// at the moment it matters. Without it the first thing a new project does is
// fail fifteen times with "missing go.sum entry for module providing package
// github.com/MerseniBilel/warren/app" — which names neither the cause nor
// the fix.
//
// Delete this when v0.1.0 is tagged and the require resolves on its own.
func unpublishedNotice(frameworkPath string) string {
	if frameworkPath != "" {
		return ""
	}
	return "Warren is not published yet, so `go mod tidy` cannot resolve it. Either\n" +
		"re-run with --framework <path-to-your-warren-checkout>, or add to go.mod:\n\n" +
		"  replace github.com/MerseniBilel/warren => /path/to/warren\n" +
		"  replace github.com/MerseniBilel/warren/transport/http => /path/to/warren/transport/http\n\n"
}

func envPrefix(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			out = append(out, r-32)
		case r == '-':
			out = append(out, '_')
		default:
			out = append(out, r)
		}
	}
	return string(out)
}
