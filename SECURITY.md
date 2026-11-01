# Security policy

## Reporting a vulnerability

Please report privately, not in a public issue. Use GitHub's private
vulnerability reporting for this repository: open the **Security** tab of
`saphan-ai/mocks-studio`, choose **Report a vulnerability**, and describe what
you found.

Include the version or commit you tested, the mock and the smallest request or
manifest that shows the problem, what you expected and what happened.

We acknowledge a report within 5 working days and tell you what we will do about
it. We ask that you give us a reasonable time to fix a problem before you
publish it, and we credit reporters who want credit.

## What is in scope

The specification, the conformance suite and the mocks in `mocks/`. In
particular, report:

- a mock that reaches the network, a real service or a path outside its own
  working directory, when the specification says it must not;
- one run's state, events or data visible to another run;
- a control operation that a mock accepts from a caller it should refuse;
- a crash, a hang or unbounded memory use caused by a crafted request;
- a mock the conformance suite passes although it breaks the specification, or a
  correct mock the suite fails;
- a place where the specification says one thing and the suite or a mock
  another.

## What is not in scope

- The Mocks Studio engine, which is not in this repository.
- The behaviour of the real systems the mocks imitate. A mock answers like the
  real system for the cases it documents, and makes no other claim.
- Credentials or data a user chose to load into a mock.

## Supported versions

The latest commit on `main`, and the latest tagged release when releases exist.
