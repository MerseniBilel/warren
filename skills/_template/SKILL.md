---
name: warren-<command-name>
description: <One line, written so a model can decide relevance without reading further. Say what the command produces and when it applies. e.g. "Generate a DDD entity with its repository interface and domain events inside a Warren module.">
---

# warren <command>

<!--
  Copy this directory to skills/warren-<command>/ and fill it in.

  Sections marked GENERATED are produced from Cobra command metadata by
  `make skills-gen`. Do not hand-edit them: `make skills-check` diffs the
  regenerated output against what is committed and fails on drift.

  Everything else is hand-written. Keep the whole file under ~200 lines — a
  skill competes for context, and a long one is usually a command doing too much.
-->

## When to use this

<!-- Be specific. The commonest agent failure is picking the wrong generator. -->

Use this when:
- <concrete situation>
- <concrete situation>

## When NOT to use this

<!-- Name the neighbouring commands explicitly and say what to use instead. -->

- <situation> — use `warren g <other>` instead.
- <situation> — this is not what that means in Warren; see [link].

## Invocation

<!-- GENERATED from Cobra metadata — do not hand-edit -->

```bash
warren <command> <args> [flags]
```

| Flag | Type | Default | Description |
|---|---|---|---|
| `--<flag>` | `string` | `<default>` | <description> |
| `--dry-run` | `bool` | `false` | Print the diff without writing |
| `--force` | `bool` | `false` | Overwrite existing files |

<!-- END GENERATED -->

## What it writes

<!--
  List every path. This is the section that stops an agent from hand-authoring
  a file the generator just created — the single most common waste.
-->

```
internal/modules/<module>/
├── domain/<name>.go            # <what is in it>
└── domain/<name>_test.go       # <what is in it>
```

## What it wires automatically

<!--
  Critical. Agents hand-edit module.go after the AST edit already did it,
  producing duplicate registrations.
-->

- Registers `<X>` in `internal/modules/<module>/module.go` via an AST edit.
- **Do not hand-edit `module.go` after running this.** Read it instead.

## Verify

```bash
warren lint arch && go build ./... && go test ./internal/modules/<module>/...
```

Expected output: <what success looks like>

## Failure modes

| Message | Means | Fix |
|---|---|---|
| `<exact error text>` | <cause> | <exact command or edit> |
| `file exists: <path>` | Already generated | Read the file; use `--force` only if replacing it deliberately |

## Example

```bash
$ warren g <command> <realistic args>
```

```
<real output, trimmed>
```

Then edit `<file>` to <the one thing a human still has to do>.

## Notes

- This generator is **idempotent**: re-running is a no-op or a clearly-marked diff.
- Generated code is **yours** — committed, editable, not regenerated behind your back.
- Preview before writing with `--dry-run`.
