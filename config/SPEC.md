# `github.com/MerseniBilel/warren/config` — SPEC

| | |
|---|---|
| **Status** | **Approved (2026-08-01)** — `Load`, `Source`, `WithEnvPrefix`, `WithFlags` implemented; flag derivation and the validate seam settled the same day (Open questions 6, 9). `Module[T]` waits for the root `warren` package to exist — it returns `warren.Module` — and is the one unimplemented item |
| **Source** | [warren.md §2.4](../warren.md) |
| **Module** | core |
| **Mode** | Build |
| **Wraps** | — |

## Problem

Configuration is the first thing every user touches, and getting it wrong is how
services die in production: a missing `WARREN_POSTGRES_DSN` becomes a
nil-pointer panic on the first query instead of a startup failure naming the
field. The obvious library, Viper, brings a heavy transitive dependency tree,
global state, and stringly-typed access into the kernel — which the dependency
budget (§1.7) and invariant 1 will not carry.

The way out is to split config by **where a value comes from**, not by what
parses it (§2.4): struct defaults, environment variables, and CLI flags need no
parser and live in core; config files need one and live in submodules, plugged
in through a core-owned port. Core never knows YAML exists — it just sees a
map.

## Goals

- Own layered configuration resolution and validation (§2.4).
- Resolve in one fixed order, later wins: **struct defaults → file sources →
  environment variables → command-line flags** (§2.4, boot step 0). Core merges
  all sources in order and binds the result to the struct.
- **Core parses no files.** Define the `Source` port in core so that file
  formats — and, later, remote sources — plug in from submodules with no change
  to core. `config/yaml` is the first such submodule (§1.6); the same hook
  later admits `config/toml`, `config/json`, or a Vault/AWS-Secrets source
  (§2.4).
- Deliver a **typed struct**, not a stringly-typed bag. Providers depend on
  `Config` as an ordinary constructor parameter (§2.4).
- **Validate at boot.** A missing required field is a startup failure with the
  field path named, not a nil-pointer panic on the first query (§2.4, §1.3's
  rule: every error the framework can detect surfaces at boot, never on
  request 1).
- Contribute itself to the graph as a module, so `Config` is injectable
  (`config.Module[Config](...)`, §2.4, §10).
- Stay small: this is ~600 lines to own (§2.4).

## Non-goals

- **Core parses no files — this package contains no parser for any format,
  permanently.** YAML, TOML, JSON, and anything else that needs parsing arrive
  as `Source` implementations from their own submodules (`config/yaml` first,
  §1.6, its YAML library TBD pending audit). This is invariant 1's stated
  move — port in core, implementation in a submodule — applied to config.
- **Not Viper.** Rejected for weight, global state, and stringly-typed access
  (§2.4, §9 ledger: "Config — Build — Viper rejected: weight + global state").
- **No stringly-typed accessor.** There is no `config.GetString("http_port")`.
  The only way out of this package is a typed `T`.
- **No package-level mutable state.** Rejecting Viper's global state is half the
  reason this package is Build; AGENT.md § General forbids it anyway.
- **Not the validator.** Field constraints are expressed with `validate:` tags
  and enforced by `warren/validate` (§2.7), which normalises failures to
  `*warren.Error` with `CodeInvalid` and per-field details.
- **Not a secrets manager, not a remote config source.** Core ships defaults,
  env, and flags, nothing more. A Vault or AWS-Secrets source is a future
  `Source` in its own submodule (§2.4) and is out of scope here.

## Public API

Amended `warren.md` §2.4 fixes the following surface. Doc comments are added
here; no signature is changed. `WithFile` is gone — files arrive as `Source`
values from submodules.

```go
// Load resolves configuration into T: struct defaults, then each file Source
// in the order given, then environment variables, then command-line flags —
// later sources win. It validates the result before returning.
func Load[T any](opts ...Option) (T, error)

// Module returns a warren.Module that loads T at boot and provides it to the
// graph, so any constructor can take T as a dependency.
func Module[T any](opts ...Option) warren.Module

// Option configures loading.
type Option // representation not fixed by warren.md; it must admit Source values

// Source is where config values come from. Defaults, env, and flags ship in
// core; anything needing a parser implements Source from its own submodule.
// A Source is itself an Option, so it slots straight into Module and Load.
type Source interface {
    Load() (map[string]any, error)
}

// WithEnvPrefix sets the prefix for environment variable lookup: prefix
// "WARREN" maps the field path postgres.dsn to WARREN_POSTGRES_DSN.
func WithEnvPrefix(prefix string) Option

// WithFlags takes the final layer from a flag set. Whether it must already be
// parsed is Open question 6.
func WithFlags(fs *flag.FlagSet) Option
```

The first file submodule, `warren/config/yaml` — a separate Go module (§1.6)
and the only place a YAML library exists — exposes:

```go
// File returns a Source that reads and parses the YAML file at path.
func File(path string) config.Source
```

Its behaviour (environment-specific files, missing-file semantics, the YAML
library it wraps) is specified in its own `SPEC.md`, not here.

### The struct contract

A user's configuration is a plain struct, and three tags carry the whole
contract (§2.4):

```go
type Config struct {
    Env      string `config:"env" default:"development" validate:"oneof=development staging production"`
    HTTPPort int    `config:"http_port" default:"8080"`

    Postgres struct {
        DSN         string `config:"dsn" validate:"required"`   // → WARREN_POSTGRES_DSN
        MaxConns    int32  `config:"max_conns" default:"10"`
    } `config:"postgres"`

    Kafka struct {
        Brokers []string `config:"brokers" validate:"required,min=1"`
        Group   string   `config:"group" validate:"required"`
    } `config:"kafka"`
}
```

- the `config:` tag names the field's key in every source's map and derives the
  environment variable name,
- `default:` supplies the first layer,
- `validate:` is enforced after resolution.

Nested structs nest their key path: `postgres` + `dsn` → `WARREN_POSTGRES_DSN`.

### Use

Both forms from §2.4 — without a file, config is core-only and adds zero
dependencies; adding a file pulls in exactly one submodule:

```go
// main.go — no file: core only, zero extra deps. Most containerized services.
warren.New(
    config.Module[Config](config.WithEnvPrefix("WARREN")),
    ...
)

// main.go — with a YAML file: pulls in the config/yaml submodule.
warren.New(
    config.Module[Config](
        yaml.File("config.yaml"),
        config.WithEnvPrefix("WARREN"),
    ),
    ...
)

// Any provider now takes Config as a dependency:
func NewUserRepository(cfg Config, pool *pgxpool.Pool) domain.UserRepository { ... }
```

## Behaviour

### Boot step 0

```
 0  load config          layered: defaults → file → env → flags, validated
```

Configuration is resolved and validated before the rest of the boot sequence
(§1.3). The order of the layers may not be rearranged.

### Sources, split by where a value comes from

§2.4 fixes the split — needing a parser is what pushes a source out of core:

| Source | Needs a parser? | Where it lives |
|---|---|---|
| Struct defaults | No | core |
| Environment variables | No | core |
| CLI flags | No | core |
| Config files (YAML, TOML, …) | Yes | submodule (`config/yaml` first) |

### Resolution

Resolution, in full (§2.4):

| Order | Layer | Comes from |
|---|---|---|
| 1 | struct defaults | `default:` tags |
| 2 | file sources | each `Source` given as an option, in the order given |
| 3 | environment | variables under the `WithEnvPrefix` prefix |
| 4 | flags | the `*flag.FlagSet` given to `WithFlags` |

Later layers overwrite earlier ones field by field. Core merges each source's
`map[string]any` in order and binds the merged result to the struct — it never
knows what format a `Source` parsed. Validation runs once, on the merged
result.

`Load` is the direct form and returns `(T, error)`. `Module[T]` is the same
resolution expressed as a module, and provides the resolved `T` to the graph so
that constructors can depend on it.

## Errors

`warren.md` fixes one behaviour and no text: *a missing `WARREN_POSTGRES_DSN`
is a startup failure with the field path named, not a nil-pointer panic on the
first query.* The wording was agreed on 2026-08-01; every non-deferred row has
a golden file in `config/testdata/`, and every resolution failure in one
`Load` is reported joined, so one boot names them all.

| Condition | Text |
|---|---|
| A required field is unset after all layers merge | `config: postgres.dsn is required and no layer set it — set WARREN_POSTGRES_DSN, add "dsn" under "postgres" in a file source, or pass -postgres.dsn` — the fix list adapts to which layers are configured (no prefix → no env clause, no flag set → no flag clause). |
| A field fails a `validate:` constraint | **Deferred with the validate seam (Open question 9):** core enforces only `required`; the rest of the `validate:` vocabulary surfaces through `warren/validate` once its seam exists. |
| A `Source` returns an error from `Load()` | `config: source 1 (yaml.File) failed: <cause>` — names the layer position and the source's type, wraps the cause with `%w`; the detail is the submodule's message. |
| A value cannot be converted to the field's type | `config: postgres.max_conns: cannot use "many" (from environment variable WARREN_POSTGRES_MAX_CONNS) as int32` — field path, layer, offending value. |
| A `default:` tag cannot be parsed into its field type | `config: port: default: tag "not-a-number" is not a valid int — fix the struct tag` |
| The flag set was not parsed | `config: the flag set given to WithFlags has not been parsed — call its Parse method before Load` |
| A non-option is passed | `config: string is not an option — pass a Source, WithEnvPrefix, or WithFlags` — the price of `Option` admitting `Source` values directly (see Open question 6's resolution). |

## Testing

- **Golden-file test for every error message** above, once the text is agreed.
  The missing-`WARREN_POSTGRES_DSN` message is the headline one, because it is
  the example `warren.md` chose.
- **Layering table test.** One fixture struct, all four layers, one subtest per
  precedence pair, asserting later wins field by field — including a field set
  by every layer at once, and two file sources where the later one wins. The
  file layer is exercised through a stub `Source` defined in the test — no
  parser, no file on disk.
- **Core-imports test.** A core-only build imports the standard library only:
  `go list -deps` over `warren/config` shows no third-party module — no YAML
  library, no parser of any kind. This is invariant 1 made checkable for this
  package. The `config/yaml` submodule's parsing tests live in **its** spec,
  later.
- **Env-name derivation table test.** `postgres.dsn` + prefix `WARREN` →
  `WARREN_POSTGRES_DSN`; nested, slice, and single-word fields.
- **Validation-at-boot test.** `Module[Config]` fails the boot before any
  handler runs when a required field is missing. Whether that failure lands at
  step 0 or at step 4 depends on the unresolved step-0-versus-module question —
  see Open questions and the root `SPEC.md`'s open question 2.
- **No network, no Docker, no sleeps.** Sources are stubs constructed in the
  test; environment is set per subtest; flags come from a constructed
  `flag.FlagSet`.
- **No global state test.** Two concurrent `Load` calls with different prefixes
  and different sources do not interfere — this is the Viper failure mode the
  package exists to avoid, so it gets a test.
- **Allocation benchmark.** Configuration is not on the request path; loading is
  a boot-time cost and needs no per-request number.

## Definition of done

- [x] Spec approved (2026-08-01 — see Status for the binding scope and
      conditions).
- [x] Remaining Open questions settled: flag derivation (6), `T` versus `*T`
      (7), the validate seam (9) — resolutions below, 2026-08-01. Question 8
      (§10's composition-time read) is the root package's and moves there.
- [x] Public API implemented exactly as in Public API above, with doc
      comments — `config/config.go` — **except `Module[T]`**, which returns
      `warren.Module` and therefore waits for the root package; it is one
      provider over `Load` once `warren` exists.
- [x] All four layers implemented in the fixed order, with a table test per
      precedence pair, the file layer driven by a stub `Source` — including
      the flag-default case (an unset flag must not beat the environment).
- [x] Every non-deferred error has agreed text; golden files in
      `config/testdata/`. All failures in one `Load` are reported joined.
- [x] No package-level mutable state, no `init()`; the no-global-state test
      runs two concurrent `Load` calls with different prefixes.
- [x] Core module `go.mod` unchanged — verified 2026-08-01: `go list -deps`
      over `warren/config` shows the standard library only, and
      `scripts/invariants.sh` holds the module to stdlib + dig.
- [ ] The `warren/config/yaml` submodule does **not** exist yet: it needs its
      own `SPEC.md` and a recorded dependency audit of its YAML library (TBD,
      §1.6) before the module is created. Its open questions (3 and 4 below)
      stay parked here until then.
- [x] `warren.md` amended in the same change — §2.4 note on `Option`'s
      runtime shape and the settled flag semantics.

## Open questions

1. **RESOLVED (2026-08-01).** The key tag is spelled **`config:`** — `koanf:`
   was a leftover from a rejected design, and `warren.md` §2.4 has been amended
   accordingly.
2. **RESOLVED (2026-08-01).** What parses `config.yaml`: invariant 1's stated
   move — **a port in core, the parser in a submodule**. Core defines
   `Source`; `warren/config/yaml` (its own Go module, §1.6, YAML library TBD
   pending audit) implements it. Core parses no files, permanently.
3. **How does `config/yaml` determine `<env>` for an environment-specific file
   such as `config.<env>.yaml`?** Core has no notion of `<env>` — it only
   merges `Source` maps. If the submodule offers environment-specific files,
   how it learns the environment (read from the environment before the layers
   merge, a parameter alongside `File`, something else) is a `config/yaml`
   decision, to be settled in that submodule's spec. Kept open here so it is
   not lost.
4. **Is a missing config file an error?** In a container there is often no file
   at all and everything comes from the environment. This is now the
   submodule's call: does `yaml.File(path)` fail on a missing file, or return
   an empty map? To be settled in `config/yaml`'s spec; core just sees a
   `Source` succeed or fail.
5. **RESOLVED (2026-08-01).** `WithFile` is **removed** from the surface. Files
   arrive as `Source` values (`yaml.File("config.yaml")`), so the question of
   what `WithFile` does to `config.<env>.yaml` dissolves with it.
6. **RESOLVED (2026-08-01) — parsed, dotted names, set-flags only.**
   `WithFlags` requires an already-parsed set (parsing arguments is main's
   business; an unparsed set is a boot error with its own golden text). Flag
   names are the dotted field path (`-postgres.dsn`), and only flags
   explicitly set on the command line override earlier layers — a flag's
   *default* must not beat the environment, or precedence inverts.
   Relatedly, `Option`'s runtime shape: the approved API says "a Source is
   itself an Option", and Go's type system cannot express an interface that
   foreign `Source` implementations satisfy alongside core's option
   functions — so `Option` is dispatched at `Load` time and a non-option
   argument is a boot error naming the type. Compile-time option safety was
   traded for the fixed ergonomics, deliberately.
7. **RESOLVED (2026-08-01) — `T`, by value, only.** §2.4's provider takes
   `cfg Config`; binding `*T` as well would create two names for one value
   in the graph. A constructor wanting a pointer takes `T` and takes its
   address.
8. **§10's composition-time read** (`cfg.Postgres.DSN` inside `warren.New`)
   — **moved to the root package's spec**, where composition order lives;
   nothing in this package changes either way.
9. **RESOLVED (2026-08-01) — split the word "validation".** The headline
   behaviour — a missing required value fails boot naming the field — is
   *resolution completeness*, not validation: it needs no validator, so core
   checks the `required` token itself, permanently stdlib-only. The rest of
   the `validate:` vocabulary (`oneof`, `min`, …) is constraint validation
   and belongs to `warren/validate`, whose own spec must resolve where its
   implementation lives (invariant 1 forbids `go-playground/validator` in
   core — port in core, implementation in a submodule is the expected
   shape). Until that seam exists, non-`required` constraints are not
   enforced, and this spec says so rather than pretending.
