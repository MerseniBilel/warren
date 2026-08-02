// Package command builds the CLI's command tree.
package command

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

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
	root.AddCommand(newCmd(), versionCmd())
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

func newCmd() *cobra.Command {
	var opts scaffold.Options
	cmd := &cobra.Command{
		Use:   "new <name>",
		Short: "Scaffold a Warren application",
		Long: "new writes a working Warren service: a module graph, a feature with\n" +
			"its four layers, the transactional outbox, a consumer, and tests that\n" +
			"boot the whole thing. It compiles and passes `go test` as generated.",
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
				"Created %s\n\n  cd %s\n  %s_NAME=%s go run ./cmd/%s\n  go test ./...\n\n"+
					"No HTTP or gRPC server yet — those adapters have not shipped. README.md\n"+
					"says what works today and where the server goes when it does.\n",
				abs, opts.Dir, envPrefix(opts.Name), opts.Name, opts.Name)
			return err
		},
	}
	cmd.Flags().StringVar(&opts.ModulePath, "module", "", "the Go module path of the new app (required)")
	cmd.Flags().StringVar(&opts.Dir, "dir", "", "where to write it (default: the app's name)")
	cmd.Flags().StringVar(&opts.Transport, "transport", "", "transport adapter (none released yet)")
	cmd.Flags().StringVar(&opts.DB, "db", "memory", "persistence driver: memory")
	cmd.Flags().StringVar(&opts.Broker, "broker", "memory", "broker driver: memory")
	return cmd
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
