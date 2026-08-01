package config_test

import (
	stderrors "errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MerseniBilel/warren/config"
)

var update = flag.Bool("update", false, "rewrite golden files")

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("writing golden file: %v", err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden file: %v", err)
	}
	if got != string(want) {
		t.Errorf("error text does not match golden file %s\ngot:  %q\nwant: %q", path, got, want)
	}
}

// stub is the in-test Source: no parser, no file on disk.
type stub struct {
	m   map[string]any
	err error
}

func (s stub) Load() (map[string]any, error) { return s.m, s.err }

// appConfig is the §2.4 fixture shape.
type appConfig struct {
	Env      string `config:"env" default:"development" validate:"oneof=development staging production"`
	HTTPPort int    `config:"http_port" default:"8080"`

	Postgres struct {
		DSN      string `config:"dsn" validate:"required"`
		MaxConns int32  `config:"max_conns" default:"10"`
	} `config:"postgres"`

	Kafka struct {
		Brokers []string      `config:"brokers"`
		Group   string        `config:"group"`
		Flush   time.Duration `config:"flush" default:"5s"`
	} `config:"kafka"`
}

func TestLayering(t *testing.T) {
	// Not parallel at the top: subtests using t.Setenv may not run in
	// parallel with anything.

	dsn := func(opts ...config.Option) string {
		t.Helper()
		cfg, err := config.Load[appConfig](opts...)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return cfg.Postgres.DSN
	}

	t.Run("defaults are the first layer", func(t *testing.T) {
		cfg, err := config.Load[appConfig](stub{m: map[string]any{"postgres": map[string]any{"dsn": "x"}}})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.HTTPPort != 8080 || cfg.Postgres.MaxConns != 10 || cfg.Env != "development" || cfg.Kafka.Flush != 5*time.Second {
			t.Errorf("defaults not applied: %+v", cfg)
		}
	})

	t.Run("a source overrides a default", func(t *testing.T) {
		cfg, err := config.Load[appConfig](stub{m: map[string]any{
			"http_port": 9090,
			"postgres":  map[string]any{"dsn": "from-source"},
		}})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.HTTPPort != 9090 {
			t.Errorf("HTTPPort = %d, want the source's 9090 over the default", cfg.HTTPPort)
		}
	})

	t.Run("a later source overrides an earlier one", func(t *testing.T) {
		got := dsn(
			stub{m: map[string]any{"postgres": map[string]any{"dsn": "first"}}},
			stub{m: map[string]any{"postgres": map[string]any{"dsn": "second"}}},
		)
		if got != "second" {
			t.Errorf("DSN = %q, want the later source's %q", got, "second")
		}
	})

	t.Run("environment overrides a source", func(t *testing.T) {
		t.Setenv("WARREN_POSTGRES_DSN", "from-env")
		got := dsn(
			stub{m: map[string]any{"postgres": map[string]any{"dsn": "from-source"}}},
			config.WithEnvPrefix("WARREN"),
		)
		if got != "from-env" {
			t.Errorf("DSN = %q, want the environment's %q", got, "from-env")
		}
	})

	t.Run("a set flag overrides the environment", func(t *testing.T) {
		t.Setenv("WARREN_POSTGRES_DSN", "from-env")
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.String("postgres.dsn", "", "")
		if err := fs.Parse([]string{"-postgres.dsn=from-flag"}); err != nil {
			t.Fatalf("parsing flags: %v", err)
		}
		got := dsn(config.WithEnvPrefix("WARREN"), config.WithFlags(fs))
		if got != "from-flag" {
			t.Errorf("DSN = %q, want the flag's %q", got, "from-flag")
		}
	})

	t.Run("an unset flag does not override anything", func(t *testing.T) {
		t.Setenv("WARREN_POSTGRES_DSN", "from-env")
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.String("postgres.dsn", "flag-default", "")
		if err := fs.Parse(nil); err != nil {
			t.Fatalf("parsing flags: %v", err)
		}
		got := dsn(config.WithEnvPrefix("WARREN"), config.WithFlags(fs))
		if got != "from-env" {
			t.Errorf("DSN = %q, want %q — a flag's default must not beat the environment", got, "from-env")
		}
	})

	t.Run("all four layers at once, the flag wins", func(t *testing.T) {
		t.Setenv("WARREN_HTTP_PORT", "7070")
		t.Setenv("WARREN_POSTGRES_DSN", "x")
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.String("http_port", "", "")
		if err := fs.Parse([]string{"-http_port=6060"}); err != nil {
			t.Fatalf("parsing flags: %v", err)
		}
		cfg, err := config.Load[appConfig](
			stub{m: map[string]any{"http_port": 9090}},
			config.WithEnvPrefix("WARREN"),
			config.WithFlags(fs),
		)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.HTTPPort != 6060 {
			t.Errorf("HTTPPort = %d, want the flag's 6060 over 9090 (source) and 7070 (env) and 8080 (default)", cfg.HTTPPort)
		}
	})
}

func TestEnvNameDerivation(t *testing.T) {
	t.Setenv("APP_ENV", "staging")
	t.Setenv("APP_POSTGRES_MAX_CONNS", "32")
	t.Setenv("APP_KAFKA_BROKERS", "a:9092, b:9092")
	t.Setenv("APP_POSTGRES_DSN", "x")

	cfg, err := config.Load[appConfig](config.WithEnvPrefix("APP"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Env != "staging" {
		t.Errorf("Env = %q — single-word field should read APP_ENV", cfg.Env)
	}
	if cfg.Postgres.MaxConns != 32 {
		t.Errorf("MaxConns = %d — nested field should read APP_POSTGRES_MAX_CONNS", cfg.Postgres.MaxConns)
	}
	if want := []string{"a:9092", "b:9092"}; !slices.Equal(cfg.Kafka.Brokers, want) {
		t.Errorf("Brokers = %v, want %v — comma-split and trimmed from APP_KAFKA_BROKERS", cfg.Kafka.Brokers, want)
	}
}

func TestMissingRequiredField(t *testing.T) {
	t.Parallel()

	t.Run("headline: names the env var, the file key, and the flag", func(t *testing.T) {
		t.Parallel()
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		if err := fs.Parse(nil); err != nil {
			t.Fatalf("parsing flags: %v", err)
		}
		_, err := config.Load[appConfig](config.WithEnvPrefix("WARREN"), config.WithFlags(fs))
		if err == nil {
			t.Fatal("Load with postgres.dsn unset returned nil")
		}
		assertGolden(t, "missing_required", err.Error())
	})

	t.Run("without env or flags, only the file fix is offered", func(t *testing.T) {
		t.Parallel()
		_, err := config.Load[appConfig]()
		if err == nil {
			t.Fatal("Load with postgres.dsn unset returned nil")
		}
		want := `config: postgres.dsn is required and no layer set it — add "dsn" under "postgres" in a file source`
		if err.Error() != want {
			t.Errorf("error = %q, want %q", err.Error(), want)
		}
	})
}

func TestConversionErrors(t *testing.T) {
	t.Setenv("WARREN_POSTGRES_MAX_CONNS", "many")
	t.Setenv("WARREN_POSTGRES_DSN", "x")

	_, err := config.Load[appConfig](config.WithEnvPrefix("WARREN"))
	if err == nil {
		t.Fatal("Load with a non-numeric value for an int32 field returned nil")
	}
	assertGolden(t, "conversion", err.Error())
}

func TestBadDefaultTag(t *testing.T) {
	t.Parallel()

	type bad struct {
		Port int `config:"port" default:"not-a-number"`
	}
	_, err := config.Load[bad]()
	if err == nil {
		t.Fatal("Load with an unparseable default: tag returned nil")
	}
	assertGolden(t, "bad_default", err.Error())
}

func TestSourceFailure(t *testing.T) {
	t.Parallel()

	cause := stderrors.New("yaml: line 3: mapping values are not allowed")
	_, err := config.Load[appConfig](stub{err: cause})
	if err == nil {
		t.Fatal("Load with a failing source returned nil")
	}
	if !strings.Contains(err.Error(), "source 1") || !stderrors.Is(err, cause) {
		t.Errorf("error does not name the source and wrap its cause: %v", err)
	}
}

func TestUnparsedFlagSet(t *testing.T) {
	t.Parallel()

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	_, err := config.Load[appConfig](config.WithFlags(fs))
	if err == nil {
		t.Fatal("Load with an unparsed flag set returned nil")
	}
	assertGolden(t, "unparsed_flagset", err.Error())
}

func TestUnknownOptionType(t *testing.T) {
	t.Parallel()

	_, err := config.Load[appConfig]("not an option")
	if err == nil {
		t.Fatal("Load with a non-option argument returned nil")
	}
	if !strings.Contains(err.Error(), "string is not an option") {
		t.Errorf("error does not name the offending type: %v", err)
	}
}

func TestNotAStruct(t *testing.T) {
	t.Parallel()

	_, err := config.Load[int]()
	if err == nil {
		t.Fatal("Load[int] returned nil")
	}
	if !strings.Contains(err.Error(), "must be a struct") {
		t.Errorf("error does not say T must be a struct: %v", err)
	}
}

func TestAllFailuresReportedTogether(t *testing.T) {
	t.Parallel()

	type multi struct {
		A string `config:"a" validate:"required"`
		B string `config:"b" validate:"required"`
	}
	_, err := config.Load[multi]()
	if err == nil {
		t.Fatal("Load returned nil")
	}
	if !strings.Contains(err.Error(), "config: a is required") || !strings.Contains(err.Error(), "config: b is required") {
		t.Errorf("one boot should name every missing field, got: %v", err)
	}
}

// TestNoGlobalState is the Viper failure mode this package exists to avoid:
// concurrent loads with different sources and prefixes must not interfere.
func TestNoGlobalState(t *testing.T) {
	t.Setenv("ONE_POSTGRES_DSN", "one-env")
	t.Setenv("TWO_POSTGRES_DSN", "two-env")

	var wg sync.WaitGroup
	results := make([]string, 2)
	errs := make([]error, 2)
	for i, prefix := range []string{"ONE", "TWO"} {
		wg.Go(func() {
			cfg, err := config.Load[appConfig](config.WithEnvPrefix(prefix))
			results[i], errs[i] = cfg.Postgres.DSN, err
		})
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Load %d: %v", i, err)
		}
	}
	if results[0] != "one-env" || results[1] != "two-env" {
		t.Errorf("concurrent loads interfered: %v", results)
	}
}
