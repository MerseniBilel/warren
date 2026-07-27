# Agent Integration

Warren ships two things for coding agents: **a skill for every CLI command**,
and **an MCP server** exposing the project's semantic model. The reasoning is in
[ADR-0008](adr/0008-agent-integration.md); this document is how to build them.

---

## 1. Why both

They address different moments.

- **A skill shapes what the agent decides to do** — before it types anything.
  This matters because agents commit to an invocation before reading `--help`,
  which is why better help text does not fix the problem.
- **The MCP server answers questions while the agent works** — "what does this
  project's DI graph look like right now?" Static text cannot answer that, and
  it is exactly where agents produce confidently wrong Warren code.

An agent with no MCP connection still benefits from skills, and most agents do
not have one configured. So skills are the baseline and MCP is the amplifier.

---

## 2. Skills

### The rule

**A command is not complete until its skill exists.** Skill, tests, and docs are
the definition of done, together. A follow-up task is how twenty commands end up
with three skills.

### Where they live

```
skills/
├── README.md
├── _template/SKILL.md        # copy this
├── warren-new/SKILL.md
├── warren-generate-module/SKILL.md
├── warren-generate-entity/SKILL.md
├── warren-lint-arch/SKILL.md
└── ...
```

Skills are embedded into the CLI binary via `embed.FS` and installed into a
user's project by:

```bash
warren skills install          # writes .claude/skills/
warren skills install --agent cursor
```

Installing into the user's project rather than requiring a global setup means a
newly-cloned Warren project is agent-ready with one command.

### Required sections

Each is present because it prevents an observed failure mode.

| Section | Prevents |
|---|---|
| **When to use / when not to** | Reaching for `g entity` when the task wants `g value-object` |
| **Exact invocation** — every flag, type, default | Flag invention |
| **What files it writes** | Re-authoring a file the generator just produced |
| **What it wires automatically** | Hand-editing `module.go` after the AST edit already did it |
| **Verification command** | Assuming success |
| **Failure modes** — message, meaning, fix | Guessing at an error |
| **Worked example** | Everything above, concretely |

### Generated vs. hand-written

The mechanical sections — invocation, flags, defaults — are **generated from the
same Cobra command metadata that produces `--help`**. The narrative sections are
hand-written.

This is what keeps skills from rotting. A flag added without updating its skill
changes the generated section, and the golden-file test fails:

```bash
make skills-check     # regenerate, diff against committed; non-zero on drift
```

### Writing style

Skills are read by a model, so:

- **Imperative and specific.** "Run `warren g entity user/User --fields ...`",
  not "you can use the entity generator."
- **Show the exact command**, not a description of it.
- **State what NOT to do**, explicitly. Negative instructions are load-bearing:
  "Do not hand-write the repository interface — `--repo` generates it."
- **Keep it short.** A skill competes for context. If it exceeds ~200 lines, the
  command is probably doing too much.

---

## 3. MCP server

```bash
warren mcp serve              # stdio, for a local agent
warren mcp serve --http :7777 # streamable HTTP
```

Built on `modelcontextprotocol/go-sdk` v1.7.0+ — the official SDK, maintained
with Google ([dependencies.md §3.9](dependencies.md)). Roots, sampling, and
logging are deprecated in the 2026-07-28 spec; Warren does not build on them.

### Scope: read-mostly, deliberately

The server is **not** a wrapper around every CLI command. An agent with shell
access can already run `warren g entity`; duplicating that over a second
protocol doubles the surface for nothing.

It exposes what a shell cannot: **the live semantic model of the project.**

### Resources — what is true about this project

| URI | Answers |
|---|---|
| `warren://modules` | What bounded contexts exist; what each imports and exports |
| `warren://modules/{name}` | One module's handlers, entities, events, providers |
| `warren://di/graph` | The resolved DI graph — providers, consumers, ambiguities |
| `warren://events` | Who publishes and who consumes each event |
| `warren://handlers` | Every use case and the transports it is exposed on |
| `warren://arch/violations` | Current `lint arch` findings, live |
| `warren://config/schema` | Config keys, types, defaults, and sources |

### Tools — what would happen if

| Tool | Why not just run the CLI |
|---|---|
| `warren_explain_di` | Traces a resolution chain; structured output beats scraped text |
| `warren_check_arch` | Structured violations the agent can act on per file |
| `warren_preview_generate` | `--dry-run` as a structured diff, so the agent decides before writing |
| `warren_find_handler` | Locates a use case and all its transport adapters at once |

### Mutation stays in the CLI

No MCP tool writes to the repository. Generation should leave a shell-history
trace and a reviewable diff. An agent silently mutating a repository over a
protocol the developer is not watching is the failure mode PRD §4.1 principle 1
warns about, in a new medium.

### Implementation notes

- The analysis backing every resource is the **same** code that powers
  `warren graph` and `warren lint arch`. The MCP layer is serialisation, not new
  analysis — if you find yourself writing a second analyser, stop.
- Results are cached by package hash; re-analysing on every request makes the
  server unusable on a large project.
- Every response includes the commit or working-tree state it was computed
  against, so an agent can tell stale data from current.
- Errors follow the PRD §8 standard: what failed, why, and the fix.

---

## 4. Testing both

| What | How |
|---|---|
| Skill generated sections | Golden-file tests; `make skills-check` fails on drift |
| Skill completeness | A test asserting every registered Cobra command has a skill with all required sections |
| MCP protocol | Tests against the SDK's in-memory transport |
| MCP resource contract | Every URI named in this document resolves against a fixture project |
| MCP determinism | The same project state produces byte-identical responses |

The completeness test is what actually enforces "a command is not complete until
its skill exists" — without it, the rule is a paragraph in a document.

---

## 5. Checklist for a new command

```
[ ] Command implemented and registered in cli/
[ ] Unit tests
[ ] Golden-file test (if it generates anything)
[ ] skills/warren-<command>/SKILL.md, all required sections
[ ] make skills-check passes
[ ] Docs page
[ ] Changelog fragment (make changelog)
[ ] Considered: does this change a warren:// resource?
```
