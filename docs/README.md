# Warren Documentation

## For contributors — start here

| Document | What it covers |
|---|---|
| [architecture.md](architecture.md) | How Warren is built: module graph, layers, ports |
| [dependencies.md](dependencies.md) | Every dependency, why it was chosen, and the adoption policy |
| [testing.md](testing.md) | The test tiers and what belongs in each |
| [agent-integration.md](agent-integration.md) | Skills and the MCP server |
| [adr/](adr/) | Decisions, with the reasoning and the rejected alternatives |
| [../CONTRIBUTING.md](../CONTRIBUTING.md) | Setup, commits, changelog, review |
| [../AGENT.md](../AGENT.md) | Instructions for AI agents |

## For users — the documentation site (planned)

Nothing user-facing is written yet, because there is nothing to document:
framework code starts at v0.1 (PRD §10). This section is the plan, so the
structure is settled before the writing starts.

### The rule that shapes everything

**PRD §8: every concept has a runnable example — 100%.**

That is a build constraint, not an aspiration. It means:

- Every code sample in the docs lives in `examples/` as a **compiling,
  tested Go program**, and is included into the page by reference rather than
  pasted.
- CI builds and tests every example on every push. **A broken example fails the
  build**, exactly like a broken test.
- No sample is written directly into a markdown file. Prose rots silently;
  compiled code does not.

This is the single highest-leverage decision in the docs plan. Frameworks lose
trust when their documentation stops compiling, and it always happens by the
same route: someone pastes a snippet.

### Information architecture

```
1. Start here
   What Warren is · What it is not · Is it for you? · Install
2. Tutorial            — one narrative, monolith to first endpoint to first test
   0 → running service (< 2 min, PRD §8)
   0 → first endpoint with a passing test (< 10 min, PRD §8)
3. Concepts            — the "why", one page each
   Modules · Providers & DI · Handlers · Lifecycle · Errors
   Domain primitives · Unit of work · Events & the outbox
4. Guides              — task-oriented, "how do I…"
   Choose an HTTP router · Add gRPC · Add a message consumer
   Write a repository · Test a module · Enforce architecture
   Extract a module into a service
5. CLI reference       — generated from Cobra; never hand-written
6. API reference       — pkg.go.dev
7. Architecture        — the layering, the dependency rule, extending Warren
8. Migration           — from Kratos / go-zero / hand-rolled · upgrade guides
9. ADRs                — published as-is from docs/adr/
```

### Two audiences, addressed separately

PRD §3.4 names two very different readers, and one voice cannot serve both:

- **Go developers sceptical of frameworks.** They need to see the generated code
  early, and to be told plainly what Warren does *not* do. Lead with the escape
  hatches; a page that hides `*http.Request` loses this reader permanently.
- **NestJS / Spring / .NET developers new to Go.** They need the mapping table
  (`@Module` → `warren.Module`) and they need Go idioms explained, not assumed.

A dedicated "Coming from NestJS" page serves the second group without taxing the
first — rather than diluting every page with both framings.

### Tooling

To be decided at v0.4, when there is enough to document. The constraint is that
it must support **including code from files** so the runnable-example rule holds;
that requirement outranks any other feature.

Candidates: VitePress, Docusaurus, Hugo + Docsy, or Astro Starlight. Whatever is
chosen, `docs/adr/` publishes unmodified — the decision record is a feature of
the project, and rewriting it for the website is how it goes stale.

### Documentation standards

- **Every page states what it will not cover** and links where that lives.
- **Errors are documented where they occur**, with their fix. PRD §8 makes error
  quality a feature; the docs are half of that.
- **Show the generated code.** PRD §1.3: if a developer cannot read it and
  understand it in five minutes, the feature is wrong — so put it on the page
  and let the reader judge.
- **Version every page** to the Warren version it describes.
- **No `TODO` in published docs.** An empty page is more honest than a
  misleading one.

### What ships when

| Milestone | Documentation |
|---|---|
| v0.1 | README, install, tutorial to first endpoint, module and DI concepts |
| v0.2 | Domain primitives, repository, unit of work, testing guide |
| v0.3 | gRPC, messaging, outbox, transport-parity guide |
| v0.4 | `lint arch`, `graph`, `doctor`, OpenAPI — **the differentiators** |
| v0.5 | Site launch, migration guides, "Coming from NestJS" |
| v1.0 | API reference complete, stability commitment, upgrade guides |

Docs are written **with** the feature, in the same pull request. A feature
merged without documentation is unfinished — the same rule as skills
([ADR-0008](adr/0008-agent-integration.md)), for the same reason.
