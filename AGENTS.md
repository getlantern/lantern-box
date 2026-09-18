- Telemetry attributes: follow rules in https://github.com/getlantern/semconv/blob/main/AGENTS.md

## Comments and Documentation

**Default: no new comment.** Add a comment only when it documents a contract, invariant, concurrency rule, error condition, zero-value behavior, or surprising rationale that is not clear from the code.

- Prefer clearer names and smaller functions over explanatory comments.
- Delete or shorten verbose comments in code you touch.
- A comment that documents a real invariant, race, or rationale still fails if it includes more detail than needed to preserve that contract.
- Do not narrate code, repeat the identifier, list visible branches, mention tickets/people, or praise the implementation.
- Documentation should only document the declaration's own contract: side effects, blocking/zero-value behavior, errors, ownership, cancellation, concurrency, or surprising pre/postconditions.
- Document the caller-relevant semantics of a sentinel error, enum, status, or constant — retryability, permanence, lifecycle — as the value's own contract, phrased as what it means rather than advice: prefer "retryable only after …" over "callers should …". The consumer behavior to avoid is a separate component's policy, not the meaning of a value callers inspect.
- Comment a literal value when it encodes a non-obvious protocol, compatibility, migration, or product invariant: say why this exact value is required and what breaks if it changes, not what the assignment does. Prefer a named constant when that rationale is reusable.
- Document a contract at the narrowest declaration that depends on it — a sentinel's meaning on the sentinel or the function that returns it, a request-shape invariant at the request construction. If a comment reads as advice or sits on the wrong declaration, restate it as that declaration's contract and move it there.
- Inline comments are for local traps only. Put them above the relevant line and keep them to one short sentence when possible.
- TODOs must say what remains and why it is not done now.

## Go Doc Comments

Follow [Go doc comment conventions](https://go.dev/doc/comment), not generic prose style.

- Exported package-level declarations need doc comments; unexported declarations usually do not.
- Place doc comments immediately above the declaration, with no blank line.
- Use complete sentences. For packages, begin `Package name ...`. For commands, begin with the command name. For funcs and methods, begin with the function or method name. For types, name the type in the first sentence (`T ...`, `A T ...`, or `An T ...`).
- Keep the first sentence short; it is the synopsis shown by `go doc` and pkg.go.dev.
- Stop after one sentence unless callers need more to use the API correctly.
- Add extra paragraphs only for non-obvious contracts: concurrency safety, zero-value meaning, ownership/lifetime, blocking behavior, errors, panics, cancellation, ordering, or compatibility constraints.
- For bool-returning functions, prefer “reports whether”; do not add “or not.”
- Use `Deprecated:` on its own paragraph for deprecations.
- Prefer executable `ExampleFoo` tests over long usage prose.
- Use lists, headings, and code blocks only when the rendered public documentation genuinely needs structure.

Before finalizing Go code, re-read every added or modified comment and remove any sentence that only restates the declaration or implementation.

## Comment Verification

After any edit that adds or modifies a comment, you MUST spawn a code-reviewer subagent with the diff before declaring the task done. The subagent applies the Comments and Documentation checklist above and reports violations, including misplaced documentation where a function comment describes another declaration’s type, fields, payload shape, or consumer behavior. Fix the violations and re-spawn until the subagent reports none.

You MUST NOT skip this by self-reviewing the diff. The point of the subagent is to review without the generation bias of the Claude that wrote the comment — a self-review by the writer is a known failure mode and does not satisfy this step.
