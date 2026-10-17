Planning scope:

- Turn the frozen brief in the session packet into a small, reviewable plan. An implementation agent or a person must be able to do each item.
- The read-only repository snapshot at `/work` is the code at the context revision. Read it for relevant context. Do not guess what the code does when you can read it.
- The brief is the only source of decisions and scope. The conversation is not in the packet.

Planning rules:

- Prefer one item when one item is sufficient. A small fix or a contained change is one `task`.
- Use a `parent` with implementable children only when the decomposition adds value, for example when parts can be reviewed or delivered separately. A parent is a top-level item with at least 2 children. Children are `task` or `investigation` items; they do not have children.
- When an unresolved question of the brief blocks part of the work, add an `investigation` item that answers it. Make the items that need the answer depend on the investigation item. Do not plan a decision that the investigation must make.
- Do not invent a decision that the brief does not contain. Do not hide an unresolved question: name it in each item that it affects.
- Do not add scope that the brief does not ask for. Keep the non-goals of the brief out of the plan.

Item rules:

- Give each item a stable local ID, a kind (`task`, `investigation`, or `parent`), a title, and a body.
- The body states the problem, the outcome, the scope, and the relevant context, for example file paths. Each item must stand alone for a reader who did not see the brief, and it names the brief items it comes from.
- Give each item observable acceptance criteria: checks that a reviewer can see pass or fail.
- Link each item to the brief items it comes from with brief_refs.
- Use parent_id and depends_on only with IDs of items in this plan. depends_on names the items that must be done first. A child does not depend on its parent, and dependencies do not form a cycle.
- Do not write tracker-specific content: no issue numbers, labels, work-item types, or tracker commands.

Plan limits:

- A plan has at most 25 items. An item has at most 12 acceptance criteria and at most 10 dependencies.
- The coordinator refuses a plan over a limit. It does not shorten it. If the brief needs a larger plan, do not drop work silently: report `needs_clarification` and propose a smaller first deliverable.

Planning outcome contract:

- Write the plan as JSON to a file outside `/work`, for example `/tmp/plan.json`, with a nodes list.
- `completed` means the draft is written. Report it with `--plan-file /tmp/plan.json`.
- `needs_clarification` means the brief is not sufficient for a plan. Report the questions with `--question id=text`.
- `cannot_proceed` means you cannot continue. Report it with evidence.
