#!/usr/bin/env python3
"""Validate .golangci.yml against golangci-lint's published v2 JSON schema.

`golangci-lint config verify` is the primary check and is what CI runs. This
script exists as a fallback for environments where the binary cannot be
installed, and as a fast pre-commit check that does not require the toolchain.

It catches the failure this config is most prone to: a linter, rule, tag, or
settings key that was renamed or removed upstream. Those fail silently at
runtime -- golangci-lint warns and carries on -- so a config can rot for months
while appearing to work.

Usage:
    python3 scripts/validate-golangci-config.py [path/to/.golangci.yml]

Exit status is 0 when the config conforms, 1 otherwise.
"""

from __future__ import annotations

import json
import sys
import urllib.request
from pathlib import Path

SCHEMA_URL = "https://golangci-lint.run/jsonschema/golangci.v2.jsonschema.json"

try:
    import yaml
except ImportError:
    sys.exit("PyYAML is required: pip install pyyaml")


def load_schema() -> dict:
    with urllib.request.urlopen(SCHEMA_URL, timeout=30) as resp:  # noqa: S310
        return json.load(resp)


def main() -> int:
    cfg_path = Path(sys.argv[1] if len(sys.argv) > 1 else ".golangci.yml")
    cfg = yaml.safe_load(cfg_path.read_text())
    schema = load_schema()
    defs = schema.get("definitions", {})

    def deref(node, depth=0):
        while isinstance(node, dict) and "$ref" in node and depth < 20:
            node = defs.get(node["$ref"].split("/")[-1], {})
            depth += 1
        return node if isinstance(node, dict) else {}

    def enum_of(node):
        """Resolve an enum through $ref, array items, and anyOf wrappers."""
        node = deref(node)
        for candidate in (node, deref(node.get("items", {}))):
            if "enum" in candidate:
                return candidate["enum"]
            for alt in candidate.get("anyOf", []):
                alt = deref(alt)
                if "enum" in alt:
                    return alt["enum"]
        return None

    errors: list[str] = []
    props = schema["properties"]

    for key in cfg:
        if key not in props:
            errors.append(f"top-level: unknown key {key!r}")

    linters = deref(props["linters"])["properties"]

    known = enum_of(linters["disable"]) or []
    for name in cfg.get("linters", {}).get("disable", []):
        if name not in known:
            errors.append(f"linters.disable: {name!r} is not a known linter")
    for name in cfg.get("linters", {}).get("enable", []):
        if name not in known:
            errors.append(f"linters.enable: {name!r} is not a known linter")

    settings = deref(linters["settings"]).get("properties", {})
    for name, value in cfg.get("linters", {}).get("settings", {}).items():
        if name not in settings:
            errors.append(f"linters.settings: {name!r} has no settings schema")
            continue
        sub = deref(settings[name]).get("properties", {})
        for key in value or {}:
            if sub and key not in sub:
                errors.append(f"linters.settings.{name}: unknown key {key!r}")

    lint_settings = cfg.get("linters", {}).get("settings", {})

    revive_rules = enum_of(defs.get("revive-rules", {})) or []
    for rule in lint_settings.get("revive", {}).get("rules", []):
        if revive_rules and rule.get("name") not in revive_rules:
            errors.append(f"revive: unknown rule {rule.get('name')!r}")

    gocritic = lint_settings.get("gocritic", {})
    for tag in gocritic.get("enabled-tags", []):
        tags = enum_of(defs.get("gocritic-tags", {})) or []
        if tags and tag not in tags:
            errors.append(f"gocritic: unknown tag {tag!r}")
    checks = enum_of(defs.get("gocritic-checks", {})) or []
    for check in gocritic.get("disabled-checks", []):
        if checks and check not in checks:
            errors.append(f"gocritic: unknown check {check!r}")

    gosec_rules = enum_of(defs.get("gosec-rules", {})) or []
    for rule in lint_settings.get("gosec", {}).get("excludes", []):
        if gosec_rules and rule not in gosec_rules:
            errors.append(f"gosec: unknown rule {rule!r}")

    exclusions = deref(linters["exclusions"]).get("properties", {})
    for key in cfg.get("linters", {}).get("exclusions", {}):
        if key not in exclusions:
            errors.append(f"linters.exclusions: unknown key {key!r}")
    presets = enum_of(exclusions.get("presets", {})) or []
    for preset in cfg.get("linters", {}).get("exclusions", {}).get("presets", []):
        if presets and preset not in presets:
            errors.append(f"linters.exclusions.presets: unknown preset {preset!r}")

    formatters = deref(props["formatters"])["properties"]
    known_fmt = enum_of(formatters["enable"]) or []
    for name in cfg.get("formatters", {}).get("enable", []):
        if name not in known_fmt:
            errors.append(f"formatters.enable: {name!r} is not a known formatter")
    fmt_settings = deref(formatters["settings"]).get("properties", {})
    for name, value in cfg.get("formatters", {}).get("settings", {}).items():
        if name not in fmt_settings:
            errors.append(f"formatters.settings: unknown formatter {name!r}")
            continue
        sub = deref(fmt_settings[name]).get("properties", {})
        for key in value or {}:
            if sub and key not in sub:
                errors.append(f"formatters.settings.{name}: unknown key {key!r}")

    for section in ("run", "issues", "output", "severity"):
        if section not in cfg:
            continue
        node = deref(props[section]).get("properties", {})
        for key in cfg[section]:
            if node and key not in node:
                errors.append(f"{section}: unknown key {key!r}")

    if errors:
        for err in errors:
            print(f"FAIL: {err}", file=sys.stderr)
        print(f"\n{len(errors)} problem(s) in {cfg_path}", file=sys.stderr)
        return 1

    print(f"OK: {cfg_path} conforms to the golangci-lint v2 schema")
    return 0


if __name__ == "__main__":
    sys.exit(main())
