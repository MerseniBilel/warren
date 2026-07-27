# Testing Strategy

Warren is a framework, so its bugs become every user's bugs. The bar is higher
than for an application, and the tests have to earn that.

Two failure modes drive everything here:

1. **A generator's templates break silently.** Nothing fails; the output is just
   wrong. Only golden files catch this.
2. **A port and its drivers drift apart.** The Kafka driver grows a behaviour
   the in-memory driver lacks, and code that passed tests fails in production.
   Only a shared contract suite catches this.

---

## 1. The tiers

| Tier | Runs | Needs | Speed | Command |
|---|---|---|---|---|
| **Unit** | Every commit | Nothing | < 10 s/module | `make test` |
| **Golden** | Every commit | Nothing | fast | part of `make test` |
| **Contract** | Every commit (memory), integration (real) | Varies | fast/slow | both |
| **Integration** | Pre-merge | Docker | minutes | `make test-integration` |
| **E2E** | Pre-release | Docker | minutes | tagged `e2e` |
| **Benchmark** | On demand + release | Nothing | varies | `make bench` |

### The separation rule

**Unit tests need no Docker, no network, no filesystem outside `t.TempDir()`,
and no sleeps.** If a test needs any of those it belongs in a higher tier and
goes behind a build tag.

```go
//go:build integration
```

This is not fastidiousness. A test suite that needs Docker to run is a test
suite people stop running, and then the fast feedback loop the whole design
depends on stops existing.

Unit tests run with `-race -shuffle=on`. Shuffling catches order dependence,
which is the failure that otherwise only ever reproduces in CI.

---

## 2. How we write unit tests

**Standard library first; `testify/require` where it genuinely pays.** The core
module has no dependencies at all ([ADR-0003](adr/0003-repo-layout.md)), so core
tests use the stdlib. Submodules may use `require` for assertion-heavy tests.
`assert` (continue-on-failure) is discouraged: a test that keeps running after
its precondition failed produces a cascade of noise around one real problem.

**Table-driven, subtests named for behaviour:**

```go
func TestEmail_Validation(t *testing.T) {
    t.Parallel()
    tests := map[string]struct {
        input   string
        wantErr error
    }{
        "rejects a missing at sign":      {input: "nope", wantErr: ErrInvalidEmail},
        "rejects an empty local part":    {input: "@example.com", wantErr: ErrInvalidEmail},
        "accepts a plus-addressed inbox": {input: "a+b@example.com"},
    }
    for name, tc := range tests {
        t.Run(name, func(t *testing.T) {
            t.Parallel()
            ...
        })
    }
}
```

Names say what the behaviour is, so a failure reads as a sentence: `rejects an
empty local part` beats `case_3`.

**Assert on behaviour, not on implementation.** A test that breaks when you
rename a private field is a maintenance tax, not a safety net.

**`t.Parallel()` by default.** It surfaces shared-state bugs in framework code,
which is exactly where they are most damaging.

**A bug fix starts with a failing test.** If it passes before the fix, it is
testing something other than the bug.

**No mocking framework.** Ports are small interfaces; hand-written fakes in
`warren/testing` are clearer than generated mocks and do not go stale.

---

## 3. Golden-file tests for every generator

PRD §9 requires this and it is not optional: **every CLI generator has a
golden-file test.** A template with a broken conditional produces output that
compiles and is wrong; only byte comparison catches it.

```
cli/testdata/
└── generate_entity/
    ├── args.txt                  # the invocation
    └── want/                     # expected tree, byte for byte
        └── internal/modules/user/domain/user.go
```

Rules:

- **`-update` regenerates; a human reads the diff.** `make golden-update` exists
  and its output message says so. An unreviewed golden update defeats the test.
- **Golden output must compile.** The test runs `go build` over the generated
  tree. Output that is byte-correct and does not compile is still broken.
- **Generated output must pass Warren's own linters**, including `lint arch`.
  We ship the architecture; our generators must not violate it.
- **Determinism is enforced.** No timestamps, no map iteration order, no
  absolute paths in output. Generators run twice in tests and must produce
  identical bytes.
- Fixtures are `-text -diff` in `.gitattributes` so no CRLF conversion can
  silently break them on Windows.

The CI matrix runs core tests on Linux, macOS, **and Windows** for this reason:
generators write files and compare bytes, so path separators and line endings
must be exercised rather than assumed.

---

## 4. Contract tests across drivers

Every port ships **one** test suite that every adapter must pass. This is how
the in-memory broker stays an honest stand-in for Kafka — the property PRD §9
depends on when it says the same consumer code runs against both.

```go
// broker/brokertest/suite.go — exported, so external drivers can prove
// themselves against the same bar.
func RunSubscriberSuite(t *testing.T, newSubscriber func(t *testing.T) broker.Subscriber)
```

```go
// broker/memory/subscriber_test.go
func TestMemorySubscriber(t *testing.T) {
    brokertest.RunSubscriberSuite(t, func(t *testing.T) broker.Subscriber { ... })
}

// broker/kafka/subscriber_test.go
//go:build integration
func TestKafkaSubscriber(t *testing.T) {
    brokertest.RunSubscriberSuite(t, func(t *testing.T) broker.Subscriber { ... })
}
```

**A new behaviour is added to the suite before it is added to a driver.** That
ordering is what keeps drivers from drifting, and it is what makes
community-owned drivers viable: the certification checklist PRD §12 promises is
this suite.

Suites exist for `Repository`, `UnitOfWork`, `Publisher`, `Subscriber`,
`Router`, and `Config`.

---

## 5. Integration tests

Behind `//go:build integration`, using `testcontainers-go` wrapped in
`warren/testing` (wrapped because it is still v0 and its API moves —
see [dependencies.md §3.8](dependencies.md)).

- **Containers are shared per package, not per test.** A container per test
  turns a two-minute suite into twenty.
- **No fixed ports.** Testcontainers assigns them; hard-coded ports fail under
  parallel CI.
- **No `time.Sleep`.** Wait on a readiness condition. A sleep is either flaky or
  slow, and usually both.
- Each test gets an isolated schema or topic, so parallel runs cannot collide.

---

## 6. What must be tested, specifically

Framework-specific risks that ordinary coverage misses:

| Area | Must prove |
|---|---|
| **DI** | A missing provider fails at **boot**, not at request time. A cycle is reported with the full chain. Error text contains the resolution chain, the requesting file, and a copy-pasteable fix (PRD §8). |
| **Lifecycle** | `OnStop` runs in reverse order. Drain completes in-flight work. A hook that panics does not skip the remaining hooks. Readiness fails at the start of drain. |
| **Transport parity** | The *same* handler produces equivalent results over HTTP, gRPC, and a consumer. This is the central claim (PRD §3.3) and it needs one test that says so directly. |
| **Error mapping** | Every semantic code maps in every transport. `exhaustive` catches the compile-time half; a table test covers the values. |
| **Outbox** | State and outbox row commit atomically. A crash between commit and publish still delivers. Redelivery is idempotent. |
| **`lint arch`** | Fixture projects with known violations produce exactly the expected findings — and clean fixtures produce none. False positives are the failure mode that kills a linter. |
| **Generators** | Idempotent: re-running is a no-op or a clear diff. AST wiring into `module.go` does not corrupt hand-written code. |

### Error messages are tested as a feature

PRD §8 makes DI error quality a headline feature, so it gets asserted like one:

```go
func TestMissingProvider_ErrorNamesTheFix(t *testing.T) {
    err := boot(t, moduleMissingRepo)
    for _, want := range []string{
        "domain.UserRepository",              // what is missing
        "requested by user.NewUserService",   // who wanted it
        "warren.Provide(postgres.NewUserRepository)", // how to fix it
    } {
        if !strings.Contains(err.Error(), want) {
            t.Errorf("error message must contain %q, got:\n%s", want, err)
        }
    }
}
```

---

## 7. Coverage

Coverage is a **diagnostic, not a target**. A hard gate produces tests written
to raise a number, and those are worse than no tests because they must be
maintained.

Guidance rather than gates:

| Area | Expectation |
|---|---|
| `domain`, `app`, `errors`, `di`, `lifecycle` | High — this is the framework's logic |
| Driver adapters | Contract suite passing matters more than line coverage |
| Generated code | Not counted |
| `cli` | Golden tests are the real measure |

**What CI does enforce:** coverage may not drop by more than 2 points in a pull
request without a stated reason. Direction is a better signal than level.

---

## 8. Benchmarks

PRD §8 sets numeric targets, so they are measured rather than asserted in prose:

| Target | Benchmark |
|---|---|
| Framework startup < 50 ms | `BenchmarkBoot` across module counts |
| Cold build < 30 s on 20 modules | CI timing on a generated fixture |
| Transport overhead | Warren handler vs. raw `net/http` handler |

Run with `benchstat` against the previous release; a regression is a release
blocker, not a note.

---

## 9. Running things

```bash
make test               # unit + golden, race, shuffled
make test-short         # no race — fast inner loop
make test-integration   # Docker required
make cover              # merged profile across modules
make bench
make golden-update      # then read the diff
```

Single test while iterating:

```bash
go test ./di/... -run TestMissingProvider -race -v
```
