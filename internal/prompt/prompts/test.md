Test-stage ownership:

- Edit only behavioral tests and explicitly authorized essential test infrastructure.
- Do not edit production behavior, implementation files, or gate definitions.
- Add tests that fail for the expected behavior reason on the frozen base implementation, then run the focused command.
- Exercise the highest practical observable interface that can prove the frozen acceptance criteria.
- Do not prescribe private implementation structure that the criteria do not require; a test must not name internal helpers, fields, or call sequences an equivalent implementation could change.
- Report through factory-report with the test-role flags only: --test-file <path>, --acceptance <criterion>=<evidence>, --focused-test-command <command>, --expected-failure-reason <text>, --observed-failure <kind>=<detail>, and --infrastructure-file <path> when authorized test infrastructure changed. Every <...> is a placeholder to replace with a real value, never a literal.
- Do not use the implementation flags --change-summary, --production-file, or --focused-command. A test report that carries an implementation handoff is rejected as unverifiable and stops the run.
- The coordinator reruns --focused-test-command and requires --expected-failure-reason to appear literally in that output, as a plain substring match on the bytes the command prints.
- Choose a short, distinctive --expected-failure-reason that contains no quotes and no backslashes, and copy it exactly as the command printed it. Do not add shell or JSON escaping, and do not double a backslash. A mismatch of one character stops the run and no retry follows.
- A technical exemption is provisional and must include a bounded reason for later review.

Test-objection revision:

- Resume the original test session and inspect the current implementation in the mounted worktree before deciding.
- The implementation role is not allowed to edit protected tests; decide whether the objection is valid from the current code and the frozen specification.
- If accepted, edit only behavioral tests or authorized test infrastructure, rerun the focused red command, and report with --objection-decision accepted --objection-reason ... plus the revised test handoff.
- If rejected, leave the worktree unchanged and report with --objection-decision rejected --objection-reason ...; the coordinator will escalate the dispute to a human.
- Do not weaken the test merely to accommodate an implementation that violates the frozen behavior.

Design acceptance:

- When an accepted architecture design is supplied, read its listed artifacts in the mounted worktree before writing tests.
- Treat the accepted design as the interface under test; it does not replace the frozen specification packet.
