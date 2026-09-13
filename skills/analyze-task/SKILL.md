---
name: analyze-task
description: Analyze an imported task and produce a solution plan (step02-analyze.md) — requirements extracted from categorized attachments, affected files found in the codebase, implementation steps, assumptions and open questions. Use with a task directory and the project repository as working directory.
---

# Analyze task

**Before anything else, read `TASK_DIR/task.md`** — the compact living summary of the task's progress. If a previous run was interrupted or the user added feedback, it reflects the latest state; continue from there rather than starting over.

## Inputs (provided in the prompt)

- `TASK_DIR` — absolute path to the task folder; may contain `step01-import.md` and `attachments/`
- Task text — in the prompt and/or in `TASK_DIR/step01-import.md` (at least one is always present)
- `DECOMPOSE` — whether the decomposition pass is enabled (`yes`/`no`)
- `REVISION` — `yes` when this is a re-run triggered by user feedback (`no` on the first pass)
- `REPO MAP` — may be present: a ranked map of the repository's files and their declared symbols, ordered by how often other files reference them. Use it to orient yourself instead of spending turns on exploratory `Grep`/`Glob`. It is a **starting point, not ground truth** — it lists top-level declarations only, may be truncated on large repos, and says nothing about behaviour: verify every file you intend to touch with `Read`/`Grep` before claiming anything about it in the plan.
- `code_search` — may be available (large indexed repositories only): semantic search over the project's code index. Use it for conceptual questions ("where are refunds handled") or when you don't know the exact identifier; prefer `Grep` when you do. Results marked `stale` are older than the working tree — re-read those files.
- CWD — the task's git worktree (checked out from the fresh base branch). **Read-only**: do not modify project files at this stage; the only files you write live in `TASK_DIR`.

## Revision mode (`REVISION: yes`)

The task was already solved once and the user asked for changes. Do NOT re-derive everything from the import artifacts. Instead:

1. Read `TASK_DIR/user-feedback.md` (the user's requested changes — the newest entry at the bottom is the current request) and the existing `TASK_DIR/task.md`, `step02-analyze.md`, `step03-refined-plan.md`, `step04-execution.md` if present, plus the already-produced code changes in the worktree (`git diff <base>...HEAD`).
2. Treat the prior solution as the starting point. Produce an **incremental** plan in `step02-analyze.md` focused on what must change to satisfy the feedback — keep what already works, list only the deltas. Skim import artifacts only to resolve a specific question the feedback raises.

## Process (first pass, `REVISION: no`)

1. Read the task text and `step01-import.md`. Requirement-changing comments override the description.
2. User-supplied images in `TASK_DIR/attachments/user/` (added via the launch form, no import categories): categorize them yourself and treat their content as requirements.
3. Import attachments, by category from the import:
   - `figma-design` → extract UI/layout requirements: read the text on them, list elements and states;
   - `project-ui-screenshot` → identify which section of the project is affected;
   - before/after pairs → turn into explicit "change X to Y" requirements;
   - documents marked important by the import annotations → read them fully now.
4. Find affected files: fan out Explore subagents to search the codebase (keep your own context small — you need conclusions, not file dumps). Produce a list of files to modify, each with a one-line justification.
5. If something must be designed from scratch — propose the new file structure.
6. Write `TASK_DIR/step02-analyze.md` (template below).
7. Update `TASK_DIR/task.md` (the compact living summary): set current stage = "анализ", fill the short plan and open questions. Keep it brief — latest state only.
8. Print on its own line, exactly: `BRANCH_DESCRIPTION: <3-5-english-words-kebab-case>` — a short meaningful slug for the branch name; the orchestrator parses it.

## Questions to the user

- **Business-logic questions** (requirement conflicts, unclear business rules, genuinely can't tell what to build): output a `QUESTIONS_JSON` block (protocol below) and end your turn.
- **Non-critical uncertainties**: make a reasonable assumption and record it in the plan's Assumptions section instead of asking.
- Technical decisions (which files, how to structure code) are yours — never ask about those.

### QUESTIONS_JSON protocol

Output a single line, then end your turn:

```
QUESTIONS_JSON: [{"type": "business", "question": "...", "options": ["...", "...", "..."], "allow_custom": true}]
```

2–3 options per question. `type` is `"business"` for business-logic questions and `"decompose"` for the decomposition choice — the orchestrator relies on it for stage statuses. The orchestrator shows the options to the user and resumes this session with the answer.

## Decomposition (only when `DECOMPOSE: yes`)

After the plan is written, assess whether the task should be split:
- No → one line in the plan with the justification, and print on its own line, exactly: `DECOMPOSE_RESULT: no-split` — the orchestrator needs it to mark the decompose stage done.
- Yes → develop 2–3 decomposition options (by layer / by feature / by MR) with a recommendation, put them in the plan, and output them as a `QUESTIONS_JSON` question with `"type": "decompose"` — the user always chooses the option.

## Final verdict marker

End your final chat message (after the plan is written and decomposition is resolved) with exactly one line:

- `PLAN_RESULT: code` — the plan requires changing project code.
- `PLAN_RESULT: no-code` — no code changes are needed (the request is answered by the plan/explanation alone, e.g. a revision that turned out to be already satisfied). The orchestrator then skips the execute and review stages, so be sure: emit `no-code` only when the plan's implementation steps are genuinely empty.

## step02-analyze.md template

```markdown
# Plan: <reference or short title>

## Problem statement
## Affected files            <- each with justification
## New files                 <- if designing from scratch
## Implementation steps      <- numbered, concrete
## Assumptions
## Open questions
## Decomposition             <- verdict + options, if the stage was enabled
## Self-review               <- appended by plan-review passes; leave empty
```
