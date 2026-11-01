# Contributing

Thank you for helping. This repository holds a specification, a conformance
suite and free mocks, so a change here is judged first by what it does to the
mocks that pass and fail the suite.

## Before you start

Open an issue for anything that changes a verdict: a new rule, a changed limit,
a new field in the manifest or the event format. A change that makes the suite
fail a mock that passes it today, or pass one it fails, is a change to the
standard and is decided by the maintainers.

Corrections, clearer wording, new mocks and fixes that bring a mock or the
suite in line with the specification need no prior discussion.

## Adding a mock

A mock lives in its own directory under `mocks/<name>/`, with its own `go.mod`,
and ships:

1. a `mocks.manifest.json` that declares it;
2. a `README.md` that says which part of the real system it imitates, and which
   part it does not;
3. a `SKILL.md` that tells an agent how to use it;
4. a green run of the conformance suite:
   `go -C conformance run ./cmd/mocks-conformance --manifest ../mocks/<name>/mocks.manifest.json`.

## Tests

- No test may need the network or a real service.
- A test that guards a refusal changes exactly the field the rule guards and
  nothing else. Show that it fails when the rule is removed.
- Do not weaken a test or the suite to make a change pass.

## Wording

The specification and the READMEs say what a mock does and what the suite
checks. They do not claim that a mock is a faithful copy of the real system
beyond the cases it documents, and they do not describe how the closed part of
Mocks Studio works. A change that adds such a claim will be asked to remove it.

## Commits and pull requests

One logical change per commit. Write the subject as `<area>: <what>` and use the
body to say why. Describe in the pull request what changed, why, and how you
checked it, including the commands you ran.

By contributing you agree that your contribution is licensed under the Apache
License 2.0 (`LICENSE`) for code and `LICENSE-docs` for documentation.
