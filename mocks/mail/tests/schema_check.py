"""A small JSON Schema checker for the subset `mocks-event-v1.schema.json` uses.

The standard library has no JSON Schema validator and the mock takes no test-only dependency, so
the events the mock really serialises are checked against the published schema file by this
subset checker: type, const, enum, pattern, min/max length, minimum, required, properties,
additionalProperties, oneOf, anyOf, not and local $ref into $defs.
"""

from __future__ import annotations

import json
import re
from pathlib import Path
from typing import Any

#: The published event schema, found from this file: mocks/mail/tests -> repository root.
SCHEMA_PATH = Path(__file__).resolve().parents[3] / "spec" / "ecosystem-v1" / "mocks-event-v1.schema.json"

_TYPES = {
    "string": lambda v: isinstance(v, str),
    "integer": lambda v: isinstance(v, int) and not isinstance(v, bool),
    "object": lambda v: isinstance(v, dict),
    "boolean": lambda v: isinstance(v, bool),
}


def load_event_schema() -> dict[str, Any]:
    return json.loads(SCHEMA_PATH.read_text(encoding="utf-8"))


def problems(value: Any, schema: dict[str, Any], root: dict[str, Any], path: str = "$") -> list[str]:
    """Every way `value` breaks `schema`, as readable lines; empty means valid."""
    if "$ref" in schema:
        name = schema["$ref"].removeprefix("#/$defs/")
        return problems(value, root["$defs"][name], root, path)
    out: list[str] = []
    if "type" in schema and not _TYPES[schema["type"]](value):
        return [f"{path}: want {schema['type']}, got {type(value).__name__}"]
    if "const" in schema and value != schema["const"]:
        out.append(f"{path}: want {schema['const']!r}")
    if "enum" in schema and value not in schema["enum"]:
        out.append(f"{path}: {value!r} not in {schema['enum']}")
    if isinstance(value, str):
        out += _string_problems(value, schema, path)
    if isinstance(value, int) and not isinstance(value, bool) and "minimum" in schema and value < schema["minimum"]:
        out.append(f"{path}: below minimum {schema['minimum']}")
    if isinstance(value, dict):
        out += _object_problems(value, schema, root, path)
    out += _combinator_problems(value, schema, root, path)
    return out


def _string_problems(value: str, schema: dict[str, Any], path: str) -> list[str]:
    out = []
    if "pattern" in schema and not re.search(schema["pattern"], value):
        out.append(f"{path}: {value!r} does not match {schema['pattern']}")
    if "minLength" in schema and len(value) < schema["minLength"]:
        out.append(f"{path}: shorter than {schema['minLength']}")
    if "maxLength" in schema and len(value) > schema["maxLength"]:
        out.append(f"{path}: longer than {schema['maxLength']}")
    return out


def _object_problems(value: dict[str, Any], schema: dict[str, Any], root: dict[str, Any], path: str) -> list[str]:
    out = [f"{path}: missing required {name}" for name in schema.get("required", []) if name not in value]
    props = schema.get("properties", {})
    for name, item in value.items():
        if name in props:
            out += problems(item, props[name], root, f"{path}.{name}")
        elif schema.get("additionalProperties") is False:
            out.append(f"{path}: unexpected property {name}")
        elif isinstance(schema.get("additionalProperties"), dict):
            out += problems(item, schema["additionalProperties"], root, f"{path}.{name}")
    return out


def _combinator_problems(value: Any, schema: dict[str, Any], root: dict[str, Any], path: str) -> list[str]:
    out: list[str] = []
    if "oneOf" in schema:
        matches = sum(1 for sub in schema["oneOf"] if not problems(value, sub, root, path))
        if matches != 1:
            out.append(f"{path}: matches {matches} of the oneOf alternatives, want exactly 1")
    if "anyOf" in schema and all(problems(value, sub, root, path) for sub in schema["anyOf"]):
        out.append(f"{path}: matches none of the anyOf alternatives")
    if "not" in schema and not problems(value, schema["not"], root, path):
        out.append(f"{path}: matches a schema it must not")
    return out
