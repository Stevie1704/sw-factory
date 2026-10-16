---
name: grilling
description: Question the user a round at a time about an idea, plan, or decision until the decisions are settled. Use when you must turn an idea into settled decisions through targeted question rounds.
---

Question the user until you reach a shared understanding. Map the idea as a **design tree**: every decision branches into the decisions that depend on it.

Work the tree in **rounds**. The **frontier** is every decision whose prerequisites are already settled: the questions you can ask _now_ without guessing at answers you have not heard yet. Ask the frontier in one round: give each question a short, stable id and your recommended answer with its reason. Then end the round and wait for the user's answers before the next round.

Keep a round manageable. When the frontier is large, ask the questions that unblock the most decisions first and leave the rest for the next round. A question whose answer depends on another question still open in this round belongs to a _later_ round, not this one.

Each round the user answers reshapes the tree: settled decisions push the frontier outward and unblock questions that depended on them. Recompute the frontier and ask the next round.

When the user changes an earlier answer, the decisions below it are no longer settled. Name each decision that depended on the old answer, revise or withdraw it, and ask again where the new answer opens a question. Never keep a conclusion that contradicts the new answer.

Finding _facts_ is your job, never the user's. When a frontier question needs a fact from the environment, such as the code or its documentation, look it up yourself before you ask; do not ask the user for anything you could read. Record what you found as evidence with its source, and keep it apart from assumptions. The _decisions_ are the user's: put each to them.

Do not invent an answer to an open question. An unknown that only work can settle, such as a measurement or a prototype, stays open and is named as needing investigation.

The tree is done when the frontier is empty, or when the remaining questions no longer change what to build: nothing important is silently assumed. Do not act on the result. Propose that the decisions are complete and let the user confirm.
