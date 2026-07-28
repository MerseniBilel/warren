# Spec: Configuration

| | |
|---|---|
| **Module** | Port in core; loader in `warren/config` |
| **Milestone** | v0.1 |
| **Status** | Draft |
| **Depends on** | [01-errors](../01-errors/spec.md) |
| **Blocks** | [06-module-and-bootstrap](../06-module-and-bootstrap/spec.md), [10-cli-new](../10-cli-new/spec.md) |
| **PRD** | §6.1, §6.6 |
| **ADRs** | None yet — see §11.1, the split may warrant one |
| **Date** | 2026-07-28 |

---

## 1. Problem

Every service needs layered configuration: defaults, then a file, then
environment variables, then flags — with the later layers winning. PRD §6.6
rejects Viper explicitly for "dependency weight and global state" and directs us
to write our own loader.

The audit ([dependencies.md §3.5](../../../docs/dependencies.md)) refined that:
`knadh/koanf` is the better reading of the same intent, because **its providers
and parsers are separate Go modules**, so a service that only reads env vars
pulls only that. Critically, it does not lowercase keys and does not couple
parsing to file extensions — both Viper behaviours that surprise people in
production.

## 2. Goals

1. **Layered resolution**: defaults → file → environment → flags, last wins,
   with the winning layer recorded per key.
2. **Struct-tag binding** into a user's own config struct, so config is a typed
   value rather than `Get("db.host")` strings scattered through the codebase.
3. **Fail at boot with the whole picture.** Every missing or malformed key is
   reported in one message, not the first one found.
4. **Explain where a value came from** — `warren doctor` (v0.4) needs this, and
   "why is this value 5432" is the most common config question there is.
5. **The core module still takes no third-party dependency.**

## 3. Non-goals

- **No global state, no package-level `Get`.** Config is provided through the
  container like anything else. This is half of PRD §6.6's objection to Viper.
- **No hot reload at v0.1.** It sounds free and it is not: every consumer of a
  value must then be reload-aware. Deferred, and the port is shaped so a
  watching loader can be added without changing the consumer side.
- **No secret management.** Values arrive from the environment; a Vault or SOPS
  provider is a submodule someone can write.
- No remote config sources at v0.1.

## 4. Public API

**Two pieces, in two modules — this is the placement rule
([dependencies.md §1.3](../../../docs/dependencies.md)) applied literally.**

The port, in the **core** module, standard library only:

```go
package config // github.com/MerseniBilel/warren/config — port only

// Source is one layer of configuration. Layers are applied in order, and a
// later layer overrides an earlier one.
type Source interface {
    // Name identifies the layer in "where did this value come from" output.
    Name() string
    // Load returns this layer's values as a flat, dot-delimited key map.
    Load(ctx context.Context) (map[string]any, error)
}

// Loader resolves layers into a typed value.
type Loader interface {
    // Into unmarshals the resolved configuration into dst, a pointer to a
    // struct, using `config:"..."` tags.
    Into(ctx context.Context, dst any) error
    // Origin reports which source supplied a key's final value. It is what
    // `warren doctor` prints and what makes a surprising value explicable.
    Origin(key string) (source string, ok bool)
}
```

The loader, in the **`warren/config`** submodule, which may depend on koanf:

```go
package koanfconfig // github.com/MerseniBilel/warren/config/koanf

func New(sources ...config.Source) config.Loader

// Sources. Each is a separate constructor so a service pulls only the koanf
// providers it uses.
func Defaults(v any) config.Source            // struct defaults, from `default:` tags
func File(path string) config.Source          // format from content, not extension
func Env(prefix string) config.Source         // WARREN_DB_HOST -> db.host
func Flags(fs *flag.FlagSet) config.Source
```

**No koanf type appears in either signature** (AGENT.md invariant 2), so the
loader is swappable and a user who dislikes koanf can implement `Loader`
themselves.

## 5. Behaviour

- **Later sources win, per key**, not per file. Setting one field in the
  environment does not discard the file's other fields.
- **Keys are dot-delimited and case-preserving.** Env var mapping is
  `PREFIX_DB_HOST` → `db.host`; a leading prefix is required so Warren never
  reads an unprefixed variable it was not given.
- **File format is detected from content**, not from the extension. A `.conf`
  file holding YAML loads.
- **Every validation failure is reported together.** A config error that reveals
  one missing key per restart is how a deploy takes six attempts.
- **Unknown keys are an error by default.** A typo'd key that is silently
  ignored is the single most expensive config failure mode — the service starts,
  looks healthy, and behaves wrongly. An explicit opt-out exists for services
  that share a config file with something else.
- **`Origin` is populated during load**, not reconstructed afterwards.
- **Loading happens once, at boot.** The resolved struct is provided to the
  container as a value.

## 6. Errors

| Condition | Code | Message |
|---|---|---|
| Required key missing | `CodeInvalid` | Every missing key at once; for each, the struct field that needs it, and the env var name and file key that would supply it |
| Value fails to parse into its field type | `CodeInvalid` | The key, the value received, the target type, and which source supplied it |
| Unknown key present | `CodeInvalid` | The key, its source, and the nearest matching known key — a typo'd `db.hots` should print `did you mean db.host?` |
| File unreadable | `CodeNotFound` | The resolved absolute path, and that a missing file is only an error when explicitly requested |
| File malformed | `CodeInvalid` | The path, the line and column, and the parser's message |
| `Into` given a non-pointer | `CodeInternal` | What was passed and the calling file |

## 7. Configuration

Self-referentially: the generated `main.go` from `warren new` wires
`Defaults → File("config.yaml") → Env("WARREN") → Flags`, in that order, and
that file is the user's to edit. The framework does not choose the layers behind
their back — PRD §4.1 principle 1.

## 8. Testing

- **Precedence matrix**: every ordered pair of sources setting the same key,
  asserting the later wins and `Origin` names it.
- **Partial override**: a file setting three keys plus an env var setting one
  leaves the other two intact.
- **Unknown-key suggestion**: `db.hots` yields `did you mean db.host?`.
- **All errors at once**: a struct with four missing required fields produces one
  error naming all four.
- **Env mapping**: prefix handling, underscores to dots, and the case where the
  prefix is absent.
- **Format detection**: YAML content in a `.conf` file, and a deliberately
  malformed file reporting a line number.
- **Contract suite**: `Source` and `Loader` get a shared suite that any
  alternative implementation runs, per
  [docs/testing.md](../../../docs/testing.md) and AGENT.md's rule that a port
  change updates the contract suite first.
- No integration tier — this reads files and the environment, both of which unit
  tests can supply.

## 9. Invariants touched

- **Invariant 1** — the reason this feature is split across two modules. PRD §6.1
  lists `warren/config` among the core packages, but koanf cannot live in core.
  **The port is core; the loader is a submodule.**
- **Invariant 2** — no koanf type in any signature, which is what keeps the
  loader swappable.

## 10. Definition of done

- [ ] Port and loader match §4, in their respective modules
- [ ] Contract suite written **before** the koanf implementation
- [ ] Unit tests per §8, `-race -shuffle=on`
- [ ] `make lint-modules` confirms core still has zero third-party requires
- [ ] koanf audit row confirmed current in `docs/dependencies.md`
- [ ] `make ci` green
- [ ] `docs/` concept page: configuration layering and `Origin`
- [ ] Runnable example in `examples/config/`
- [ ] Changelog fragment

## 11. Open questions

1. **Does the port/loader split need an ADR?** PRD §6.1 lists `warren/config` as
   a core package and this spec contradicts that in order to satisfy invariant 1.
   The contradiction is already implicit in
   [dependencies.md §3.5](../../../docs/dependencies.md), but an implicit
   contradiction between two documents is exactly what an ADR is for. **Leaning
   yes** — write it with [06-module-and-bootstrap](../06-module-and-bootstrap/spec.md),
   which faces the same question about `di`.
2. **Is koanf pulling its weight at v0.1?** The v0.1 surface is env + one file +
   flags, which is a few hundred lines of standard library. Koanf earns its place
   at v0.2+ (more formats, more providers). Worth a measurement before the
   dependency is committed, not after.
3. **Should unknown keys be an error by default?** Argued yes in §5. The counter
   is a shared config file where another tool owns some keys — handled by the
   opt-out, but confirm against the dogfooding service before locking it in.
