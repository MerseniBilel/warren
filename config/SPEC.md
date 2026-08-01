# `github.com/MerseniBilel/warren/config` — SPEC

| | |
|---|---|
| **Status** | **Approved (2026-08-01)** — core surface (`Load`, `Module`, `Source`, `WithEnvPrefix`, `WithFlags`) and the parses-no-files split are binding; conditions: remaining Open questions (flag derivation, Module-vs-step-0, the validate seam) settled before those parts are implemented |
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

`warren.md` fixes one behaviour and no text: *a missing `WARREN_POSTGRES_DSN` is
a startup failure with the field path named, not a nil-pointer panic on the first
query.* All wording below is **open** and must be pinned before implementation.

| Condition | Text |
|---|---|
| A required field is unset after all layers merge | **Open.** Must name the field path (`postgres.dsn`), the environment variable that would set it (`WARREN_POSTGRES_DSN`), and the `config:` key a file source could supply. |
| A field fails a `validate:` constraint | **Open.** Surfaces through `warren/validate` as `*warren.Error` with `CodeInvalid` and per-field details (§2.7). |
| A `Source` returns an error from `Load()` | **Open.** Core reports which source failed and wraps its error with `%w`; the detail (path, parse position) is the submodule's message. Whether a missing file is an error at all is the submodule's call — Open question 4. |
| A value cannot be converted to the field's type | **Open.** Must name the field path, the source layer, and the offending value. |
| A `default:` tag cannot be parsed into its field type | **Open.** This is a programming error in the user's struct and should name the field. |

Per AGENT.md § Errors, each message must tell the user how to fix the problem —
for this package that means printing the exact variable name or config key to
set.

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
- [ ] Remaining Open questions settled before their parts are implemented:
      flag derivation (6), `T` versus `*T` (7), the §10 composition-time read
      (8), the validate seam (9).
- [ ] Public API implemented exactly as in Public API above, with doc comments.
- [ ] All four layers implemented in the fixed order, with a table test per
      pair, the file layer driven by a stub `Source`.
- [ ] Every error has agreed text and a golden-file test.
- [ ] No package-level mutable state, no `init()`.
- [ ] Core module `go.mod` unchanged — stdlib + `go.uber.org/dig` only, proven
      by the core-imports test.
- [ ] The `warren/config/yaml` submodule does **not** exist yet: it needs its
      own `SPEC.md` and a recorded dependency audit of its YAML library (TBD,
      §1.6) before the module is created. Its open questions (3 and 4 below)
      move into that spec.
- [ ] `warren.md` amended in the same change if any signature diverged.

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
6. **Does `WithFlags` require an already-parsed `*flag.FlagSet`, and how are flag
   names derived from field paths?** §2.4 fixes the signature and nothing else.
7. **Does `Module[T]` provide `T` or `*T`?** §2.4's provider takes `cfg Config`
   by value; whether a pointer is also bound is unstated.
8. **§10's `main.go` reads `cfg.Postgres.DSN` and `cfg.Kafka.Brokers` inside the
   `warren.New(...)` call**, while §2.4's pattern is that `Config` is injected
   into providers. Where does that `cfg` come from at composition time? If the
   answer is "a separate `config.Load[Config]()` call before `New`", the config
   is resolved twice and §10 should say so.
9. **Validation depends on `warren/validate`, which wraps
   `go-playground/validator/v10` (§2.7) and sits in the core module (§1.6) —
   which invariant 1 says is stdlib + dig only.** Is `validate` an accepted
   exception, its own submodule, or does config validate against a core-owned
   port? This blocks the validation path.
