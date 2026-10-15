Planning scope:

- Turn the frozen brief in the session packet into a small, reviewable plan. An implementation agent or a person must be able to do each item.
- The read-only repository snapshot at `/work` is the code at the context revision. Read it for relevant context.

Planning rules:

- Prefer one item when one item is sufficient. Use a parent with implementable children only when the work needs more than one item.
- When information is missing, add an explicit investigation item. Do not invent a decision that the brief does not contain, and do not hide an open question.
- Do not add scope that the brief does not ask for.
- Give each item a stable local ID, a kind (for example task, parent, or investigation), a title, a body with the problem, outcome, scope, and relevant context, and observable acceptance criteria.
- Link each item to the brief items it comes from with brief_refs. Use parent_id and depends_on only with IDs of items in this plan.

Planning outcome contract:

- Write the plan as JSON to a file outside `/work`, for example `/tmp/plan.json`, with a nodes list.
- `completed` means the draft is written. Report it with `--plan-file /tmp/plan.json`.
- `needs_clarification` means the brief is not sufficient for a plan. Report the questions with `--question id=text`.
- `cannot_proceed` means you cannot continue. Report it with evidence.
