# Format trajectory

The parser targets `model.Workflow`, an internal IR. The GitHub Actions YAML
frontend is one frontend, not the definition. Nothing downstream of
`internal/workflow` knows that YAML exists.

**GHA ingest is supported forever.** Migration is never forced, and a repo can
run both formats side by side. This document is about what a second frontend
would fix, not about deprecating the first.

## What the IR already separates

- Expressions survive as `model.Expr` rather than as evaluated strings, because
  most of them cannot be evaluated until earlier jobs have produced outputs.
  A frontend with real typed inputs can emit an `Expr` that is already a
  literal.
- `model.RetryPolicy` is first-class on jobs and steps. GHA has no retry
  primitive at all; the field exists in the IR regardless of which frontend
  produced it.
- `model.Deviation` lets a frontend record where it knowingly differs, so the
  UI can surface it at the point it matters.
- `model.FailureClass` means the execution semantics already distinguish
  infrastructure from user error. A native format can let an author *declare*
  which failures of their own step are infra.

## What a native format would fix

**Real typed inputs.** GHA's `workflow_dispatch` inputs are typed in the schema
and stringly-typed everywhere else; `inputs.count` arrives as `"3"`. A native
frontend emits genuine types into the IR, and the evaluator stops needing GHA's
loose coercion rules for values that were never ambiguous.

**No string-templating a shell.** `run: echo ${{ github.event.head_commit.message }}`
is a shell injection with a YAML accent, and the standard advice — assign to an
env var first — is a workaround for the format's mistake. A native format passes
values to a step as arguments or environment, never by splicing text into a
script.

**First-class retry and backoff.** Already in the IR; a native frontend makes it
the obvious way to express "this flakes on the network" instead of a
hand-rolled `for i in 1 2 3` loop.

**Explicit infra-vs-user failure semantics.** A step that shells out to a
registry could declare that a 5xx from it is infrastructure, rather than relying
on the classifier's pattern rules to infer it. The rules stay as the default for
everything undeclared.

**Composition that is not text substitution.** Reusable workflows and composite
actions currently compose by inlining and re-evaluating strings. The IR is a
graph; a native format can compose graphs.

## Constraint on every frontend

The rule that does not bend: **a key the frontend does not implement fails the
run with `unsupported: X`.** Silently ignoring input is what makes a CI system
untrustworthy, and it is not more acceptable in a format we designed than in one
we inherited.
