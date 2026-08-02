package command

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/MerseniBilel/warren/cli/internal/generate"
)

// generateCmd is `warren g`. Every subcommand does the same two things —
// write files, wire them into the module that owns them — so they share one
// runner rather than five copies of the flag handling.
func generateCmd() *cobra.Command {
	var opts generate.Options

	g := &cobra.Command{
		Use:     "generate",
		Aliases: []string{"g"},
		Short:   "Generate a module, an aggregate, a use case, a repository, or a consumer",
		Long: "generate writes the code a feature needs AND wires it into the module\n" +
			"that owns it — the second half is the part a template alone cannot do,\n" +
			"and the part people forget.\n\n" +
			"Nothing is ever overwritten: a name that collides is an error listing\n" +
			"every file involved, and no module.go is touched. Run it twice and the\n" +
			"second run refuses; there is no half-generated state to clean up.\n\n" +
			"The usual order is module → entity → repository → command.",
	}
	g.PersistentFlags().StringVar(&opts.Dir, "dir", ".", "the project root (the directory holding go.mod)")
	g.PersistentFlags().BoolVar(&opts.DryRun, "dry-run", false, "print what would happen and write nothing")
	g.PersistentFlags().BoolVar(&opts.Force, "force", false, "overwrite files that already exist")

	// run adapts a generator to cobra: parse the positional arguments, call
	// it, print the plan.
	run := func(fn func(generate.Options) (string, error), wantModule bool) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			if wantModule {
				opts.Module, opts.Name = args[0], args[1]
			} else {
				opts.Name = args[0]
			}
			plan, err := fn(opts)
			if err != nil {
				return err
			}
			verb := "Generated"
			if opts.DryRun {
				verb = "Would generate"
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s:\n\n%s\n", verb, plan)
			return err
		}
	}

	moduleCmd := &cobra.Command{
		Use:   "module <name>",
		Short: "A feature module with its four layers, registered in main",
		Long: "module creates internal/modules/<name> with its domain, application\n" +
			"and infrastructure layers, and adds it to the warren.New(...) call in\n" +
			"main.go. The layers ship with a doc.go so they exist in git — an empty\n" +
			"directory does not, and `warren lint arch` cannot check what is not\n" +
			"there.",
		Args: cobra.ExactArgs(1),
		RunE: run(generate.Module, false),
	}
	moduleCmd.Flags().StringVar(&opts.Main, "main", "",
		"the main.go to register the module in (required when a project has several)")

	entity := &cobra.Command{
		Use:   "entity <module> <Name>",
		Short: "An aggregate, its identity, its first event, and its repository port",
		Long: "entity writes the aggregate into the module's domain layer.\n\n" +
			"It wires nothing, on purpose: an aggregate is not a provider, and the\n" +
			"port it declares has no implementation until you run `warren g\n" +
			"repository`.",
		Args: cobra.ExactArgs(2),
		RunE: run(generate.Entity, true),
	}

	command := &cobra.Command{
		Use:     "command <module> <Name>",
		Aliases: []string{"usecase"},
		Short:   "A use case, its test, and its registration",
		Long: "command writes an app.Handler into the module's application layer,\n" +
			"wrapped in Transactional, together with a test that drives it through a\n" +
			"hand-written double — a generator that skips the test teaches skipping\n" +
			"the test.",
		Args: cobra.ExactArgs(2),
		RunE: run(generate.Command, true),
	}

	repository := &cobra.Command{
		Use:   "repository <module> <Name>",
		Short: "A repository implementing the aggregate's port",
		Long: "repository writes an implementation of the port the aggregate\n" +
			"declares, and provides it from the module. Run `warren g entity`\n" +
			"first: the port has to exist for this to compile.\n\n" +
			"--driver postgres writes plain SQL over postgres.DB, plus the\n" +
			"migration for its table. It follows three rules the compiler cannot\n" +
			"enforce — RequireTx first on every write, db(ctx) for the handle,\n" +
			"and persistence.Track after a successful write — which is exactly\n" +
			"why it is generated rather than described in a document.",
		Args: cobra.ExactArgs(2),
		RunE: run(generate.Repository, true),
	}
	repository.Flags().StringVar(&opts.Driver, "driver", "memory", "the repository implementation: memory or postgres")

	consumer := &cobra.Command{
		Use:   "consumer <module> <EventName>",
		Short: "An event handler and the subscription that feeds it",
		Long: "consumer writes two files. The handler is an ordinary use case in the\n" +
			"application layer and imports no transport package. The subscription\n" +
			"sits beside module.go — the only file in a feature permitted to see\n" +
			"both the broker and the handler — and assembles the full consumer\n" +
			"pipeline: recover, drain, trace, dedupe, dead-letter, retry, limit.\n\n" +
			"The topic defaults to the event name in dotted lower case:\n" +
			"OrderPlaced subscribes to order.placed.",
		Args: cobra.ExactArgs(2),
		RunE: run(generate.Consumer, true),
	}
	consumer.Flags().StringVar(&opts.Topic, "topic", "", "the topic to subscribe to (default: derived from the event name)")

	g.AddCommand(moduleCmd, entity, command, repository, consumer)
	return g
}
