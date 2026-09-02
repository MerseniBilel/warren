# `warren g consumer` — SPEC (the generator teaches the shape the framework replaced)

| | |
|---|---|
| **Status** | **APPROVED 2026-09-02 → IMPLEMENTED, PENDING REVIEW; retires on merge.** Confirmed independently three times: a Go 1.27 audit of the templates, field test #14, and field test #15, which found one generated project containing both idioms and called it the single most confidence-destroying thing it saw. Changes generated output; no user code. The blocker open question below is ANSWERED, by test. |
| **Source** | Field test #14 (`fieldtest14/REPORT.md`, finding 6); Go 1.27 audit finding Q. Verified against `cli/internal/generate/templates/subscription.go.tmpl`, `cli/internal/generate/generate.go:530-575`, and `cli/internal/scaffold/templates/internal__modules__notification__module.go.tmpl:40-70` |
| **Module** | `github.com/MerseniBilel/warren/cli` — build-time only |
| **Mode** | Build |
| **Retires** | Deleted when implemented and reviewed. |

## Problem

`warren g consumer <feature> <Event> --topic t` writes an 82-line
`on_<event>_subscription.go` that hand-assembles `broker.Pipeline`, its own
`json.Unmarshal`, and a `lifecycle.Hook` over `context.WithoutCancel`, and
wires it with `warren.Providers(...)` + `warren.Eager[...]()`.

`generate.go:542-548` states the reason:

> Providers plus Eager, not Consumers: the generated subscription wires its own
> pipeline and lifecycle hook rather than registering through `r.OnEvent`, so
> it is not a `transport.Controller`.

That reason expired. `transport.Registrar.OnEvent` is a generic method
(`transport/transport.go:738`), `broker/memory/module.go:55,71` claims
`ProtocolEvent` and serves `Table.Events()`, and **the scaffold has already
migrated**: `internal__modules__notification__module.go.tmpl:40-70` uses
`warren.Consumers(NewConsumer)` and a one-line `Register`, with a comment
reading *"88 lines that are now one line in Register."*

The generated file's own comment still says:

> When the transport adapters ship, this becomes one line in a Register
> method — `r.OnEvent(topic, h)` — and the pipeline is assembled for you.

They shipped at v0.2.0. Field test #14's result: one `notification` package
containing **both mechanisms side by side, each with a doc comment describing
the other as obsolete.** The engineer's note — *"a team copies whichever it
read last"* — is the whole severity.

warren.md §6 calls the CLI "the discovery engine"; this is the generator most
likely to be run by someone learning the framework.

## Secondary consequence, and it is the load-bearing one

The hand-rolled subscription **never enters `transport.Table`**. It is
therefore invisible to `Claim`/`Unserved`, to `warren lint`, to the OpenAPI
emitter, and to every future tool that reads the route table. A generator that
produces runtime-correct code outside the framework's own registry is a
generator that produces a blind spot.

## Ruling — generate the `Consumers` shape, delete the second file

`warren g consumer` writes **one** file — the handler under `application/` —
and edits `module.go` to add:

```go
warren.Providers(application.NewOn<Event>Handler),
warren.Consumers(New<Feature>Consumer),
```

adding the `Consumer` type and its `Register` to the feature package if it
does not already carry one, and a `r.OnEvent(topic, c.handle)` line to the
existing `Register` if it does. The second case is new: a feature consuming
two topics gets two `OnEvent` lines in one `Register`, not two files.

`on_<event>_subscription.go`, the `warren.Eager[...]()` edit, and the
`importAdapter` edit that exists only to inject `inbox.Store` and
`broker.Publisher` all go away — the module's `Register` needs neither.

### The open question this spec must settle before code

`broker.Pipeline` takes six `SubscribeOption`s: `WithRetry`,
`WithDeadLetter`, `WithConcurrency`, `WithDedupeTTL`, `WithoutDedupe`, and the
subscription name that scopes the dedupe key. `r.OnEvent(topic, h, opts...)`
takes `...broker.SubscribeOption` — so five of the six pass through unchanged.

**ANSWERED 2026-09-02, by test, and the answer is that the derived name is
per-SUBSCRIPTION.** `handlerName` (`transport/transport.go:934`) prefixes the
MODULE — `module + "." + handler` — and `broker/consumer.Serve` passes
`EventRoute.Name` straight into `broker.Pipeline` as the dedupe scope. Two
features consuming one topic therefore get two names and two scopes.
`TestTwoFeaturesOnOneTopicGetDistinctSubscriptionNames` registers `loan.opened`
from `catalog` and from `lending` and asserts the two names differ and each
carries its module; it fails if the derivation is ever changed to key on the
topic. No name option is needed, and the migration carries none.

**The sixth is the subscription NAME.** The generated pipeline sets it
explicitly:

```go
const name = "{{.Feature}}.{{.Snake}}"
```

with the comment *"It scopes the deduplication key, which is what lets another
feature consume this same topic without one of them suppressing the other."*
`OnEvent` derives the name from the module and handler instead
(`transport.go:738-776`). **Before this change lands, someone must verify that
the derived name is per-subscription and not per-topic** — if two features
subscribe to one topic and `OnEvent` gives them one dedupe scope, this change
introduces the exact defect the generated comment warns about, and the
migration must carry a name option instead.

This is stated as a blocker, not a risk: the field test's `notification`
feature had one subscription, so it did not exercise it.

## What goes red

- Three golden files under `cli/internal/generate/testdata/golden/`
  (`…__billing__module.go.golden` and the two subscription goldens) plus any
  compile test that builds a generated consumer.
- `cli/internal/generate/compile_test.go`, which compiles generated output.
- No user code: this changes what is generated next, not what was generated
  before. A project already carrying a hand-rolled subscription keeps working
  — it is valid code — which is why no migration is owed.

## Definition of done

- [x] The dedupe-scope question above is answered, in writing, with a test that
      subscribes two features to one topic.
      `transport`'s `TestTwoFeaturesOnOneTopicGetDistinctSubscriptionNames`.
- [x] `warren g consumer` on a feature with no consumer writes the consumer
      type, `Register`, and the `warren.Consumers` edit. Golden test:
      `testdata/golden/internal__modules__billing__on_payment_received_consumer.go.golden`
      and `…__billing__module.go.golden`.
- [x] The generated feature package imports neither `broker`, `inbox`,
      `lifecycle` nor `encoding/json`. `TestGenerateConsumer` asserts the
      absence of `broker.Pipeline`, `lifecycle.Hook`, `json.Unmarshal` and
      `sub.Subscribe` by name.
- [x] `warren new` followed by `warren g consumer` produces exactly one
      consumer idiom in the tree — asserted, not eyeballed:
      `TestGenerateConsumer` fails if `module.go` still carries
      `warren.Eager` or the word `Subscription`.
- [x] `.claude/skills/warren-generate/SKILL.md:24` already claims this command
      wires `warren.Consumers(...)`. It is true now; the line stays.

Added while implementing, and the reason this was a blocker rather than a
matter of taste:

- [x] A generated consumer ENTERS THE FROZEN ROUTE TABLE.
      `cli/internal/generate/compile_test.go`'s
      `TestGeneratedConsumerEntersTheRouteTable` boots the generated module and
      asserts `payment.received` is in `Table.Events()`. A `Providers`+`Eager`
      consumer runs correctly and is invisible to `openapi` and to every tool
      that reads the table; no behavioural test of the consumer catches that.

NOT done, and deliberately out of scope: **a feature that already has a
consumer still gets a second consumer type rather than an appended `OnEvent`
line.** The spec asked for the append; it is a second astedit operation
(find the existing `Register`, add a statement) and is not needed to close
either finding. Recorded here so it is a decision, not an omission — it goes
on the v0.3 list.
