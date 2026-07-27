# ADR-0008: Ship a skill per CLI command, and an MCP server for the framework

- **Status:** Accepted
- **Date:** 2026-07-27
- **Relates to:** PRD §7 (CLI), PRD §8 (DX targets)

## Context

A meaningful share of Go written from now on is written with a coding agent in
the loop. For a framework, that changes what "good developer experience" means:
the CLI's real audience is a developer *and* the agent working alongside them.

Agents fail with an unfamiliar CLI in a specific, observable way. They guess
flags. They invent subcommands that sound plausible. They run a generator, do
not read what it produced, and then hand-write a duplicate of the file it just
created. They ignore idempotency and rerun destructive commands. None of this is
fixed by better `--help` text, because the agent has usually already decided
what to type before it reads help.

Two mechanisms address different halves of this, and Warren should ship both.

## Decision

### 1. Every CLI command ships a skill

**A command is not complete until its skill exists.** The skill is part of the
command's definition of done, alongside its tests and its docs — not a follow-up
task, because a follow-up task is how twenty commands end up with three skills.

Skills live in `skills/` in this repository, are embedded into the binary via
`embed.FS`, and are installed into a user's project by `warren skills install`,
which writes them into `.claude/skills/`.

**A Warren skill states, at minimum:**

| Section | Why it exists |
|---|---|
| **When to use / when not to** | Stops an agent reaching for `g entity` when it wants `g value-object` |
| **Exact invocation** | Every flag, with types and defaults. Kills flag invention. |
| **What files it writes** | So the agent reads them instead of re-authoring them |
| **What it wires automatically** | The commonest agent failure is hand-editing `module.go` after the AST edit already did it |
| **Verification command** | The agent should confirm, not assume |
| **Failure modes** | What the error means and what to do, mapped to the PRD §8 error standard |

Skills are **generated from the same Cobra command metadata that produces
`--help`**, with hand-written narrative sections layered on top. A flag added to
a command without updating its skill is caught by CI, because the generated
portion will differ.

### 2. An MCP server for the framework

`warren mcp serve` exposes Warren to any MCP-capable agent, built on
`modelcontextprotocol/go-sdk` v1.7.0+ — the official SDK, maintained with
Google. Note that roots, sampling, and logging are deprecated in the 2026-07-28
spec; Warren does not build on them.

The MCP server is **not** a wrapper around every CLI command. Duplicating the
CLI over a second protocol doubles the surface with no gain — an agent with
shell access can already run `warren g entity`. The server exists to expose what
a shell cannot: **the live semantic model of the project.**

**Resources** (read-only project truth):

| Resource | What it answers |
|---|---|
| `warren://modules` | What bounded contexts exist, and what each imports and exports |
| `warren://di/graph` | The resolved DI graph — providers, consumers, ambiguities |
| `warren://events` | Who publishes and who consumes each event |
| `warren://handlers` | Every use case and the transports it is exposed on |
| `warren://arch/violations` | Current `lint arch` findings, live |
| `warren://config/schema` | The project's config keys, types, and defaults |

**Tools** (things a shell genuinely cannot do well):

| Tool | Why not just use the CLI |
|---|---|
| `warren_explain_di` | Traces a resolution chain; structured output beats scraped text |
| `warren_check_arch` | Returns structured violations an agent can act on per-file |
| `warren_preview_generate` | `--dry-run` as structured diff, so the agent can decide before writing |
| `warren_find_handler` | Locates a use case and all its transport adapters at once |

The distinction: **resources answer "what is true about this project," tools
answer "what would happen if."** Mutation stays in the CLI, where it is visible
in the shell history and reviewable in a diff.

### Both are tested

- Skills: golden-file tests over the generated sections. A drifted skill fails CI.
- MCP: protocol-level tests against the SDK's in-memory transport, plus a
  contract test asserting every resource URI in the docs actually resolves.

## Consequences

### What this buys

- The `warren` CLI becomes something an agent uses correctly on first attempt,
  which for the PRD §3.4 secondary audience (developers new to Go) is close to
  the whole value proposition.
- The MCP resources are largely the same analysis that already powers
  `warren graph` and `warren lint arch` (PRD §7.4). The marginal cost is a
  serialisation layer, not new analysis.

### What this costs

- ~20 skills to write and keep current. Generating the mechanical sections from
  Cobra metadata is what makes this sustainable; without that it would rot.
- An MCP server is a second public interface with its own compatibility surface.
  Scoping it to read-mostly keeps that surface small.
- The MCP spec is moving. Pinning the SDK and tracking its deprecations is
  ongoing work.

### What we now cannot do

- We cannot add a CLI command quickly and document it later. That is the point.

## Alternatives considered

**Rely on `--help` alone** — free. Rejected: it does not address the actual
failure mode, which is that agents commit to an invocation before reading help.

**MCP server only, no skills** — one mechanism instead of two. Rejected because
they serve different moments: a skill shapes what the agent decides to do, the
MCP server answers questions once it is working. An agent with no MCP connection
still benefits from skills, and most do not have one configured.

**Skills only, no MCP server** — cheaper. Rejected because static text cannot
answer "what does this project's DI graph look like right now," and that is
exactly where agents produce confidently wrong Warren code.

**Mutating MCP tools that generate code** — rejected deliberately. Generation
should leave a shell-history trace and a reviewable diff. An agent silently
mutating a repository over a protocol the developer is not watching is the
failure mode PRD §4.1 principle 1 warns about, in a new medium.

## Revisit when

- The MCP spec deprecates something Warren's server depends on.
- Skill maintenance measurably slows command development — if so, generate more.
- Evidence arrives that agents need mutating MCP tools to be effective.
