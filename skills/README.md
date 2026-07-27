# Warren Skills

One skill per CLI command, so that coding agents drive the `warren` CLI
correctly on the first attempt instead of guessing flags and inventing
subcommands.

The design and reasoning are in
[ADR-0008](../docs/adr/0008-agent-integration.md); the authoring guide is
[docs/agent-integration.md](../docs/agent-integration.md).

## The rule

**A command is not complete until its skill exists.** Skill, tests, and docs
land together with the command — not afterwards.

## Layout

```
skills/
├── README.md
├── _template/SKILL.md          # copy this to start
└── warren-<command>/SKILL.md
```

## Adding a skill

```bash
cp -r skills/_template skills/warren-generate-entity
$EDITOR skills/warren-generate-entity/SKILL.md
make skills-gen                 # fill the GENERATED sections from Cobra metadata
make skills-check               # verify no drift; CI runs this
```

## How users install them

Skills are embedded in the CLI binary and written into a project on demand:

```bash
warren skills install                  # → .claude/skills/
warren skills install --agent cursor
```

## Keeping them honest

Two mechanisms, because a documentation rule alone would not survive:

- **`make skills-check`** regenerates the mechanical sections from Cobra
  metadata and fails on any difference. Adding a flag without updating its skill
  breaks the build.
- **A completeness test** asserts every registered command has a skill
  containing every required section.
