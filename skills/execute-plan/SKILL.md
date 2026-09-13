---
name: execute-plan
description: Implement a solution plan in the task's git worktree — step by step with WIP commits, squashed into one properly named commit at the end. Use with a task directory, a commit reference, and the task worktree as working directory.
---

# Execute plan

**Before anything else, read `TASK_DIR/task.md`** — the compact living progress summary. If a previous run was interrupted or the user requested changes, it reflects the latest state and what's already committed; continue from there.

## Inputs (provided in the prompt)

- `TASK_DIR` — contains the solution plan and requirements. **The plan is `step03-refined-plan.md` if it exists (error-work stage ran), otherwise `step02-analyze.md`.** Requirements: `step01-import.md` (if import ran).
- `REFERENCE` — commit/branch reference, e.g. `tn/core/tradernet#42546`
- `TITLE` — the branch description as a normal sentence (e.g. `Add custom name id to CPS settings`)
- `BASE` — the base commit SHA recorded by the orchestrator when the worktree was created; required for the final squash. Do NOT derive it from `git rev-parse HEAD` — after a stage restart HEAD may already contain earlier WIP commits, and squashing to it would leak them into the final branch.
- `code_search` — may be available (large indexed repositories only): semantic search over the project's code index, for finding existing patterns and call sites when you don't know the exact identifier. `Grep` stays better for exact names. Results marked `stale` must be re-read from disk.
- CWD — the task's git worktree (isolated; edit freely)

## Process

1. Read the plan — the prompt may already embed its text in a `<<<PLAN ... PLAN>>>` section (use it, no need to Read the file); skim the requirements to keep them in view. The plan is the source of truth for WHAT to build; the codebase is the source of truth for HOW.
2. Confirm the base: `git merge-base --is-ancestor "$BASE" HEAD` must succeed; if it doesn't, stop and report instead of guessing.
3. Work through the plan steps in order. After each completed logical step:
   `git add -A && git commit -m "WIP: <short step description>"` — these are recovery points for pause/crash.
4. If reality contradicts the plan (file moved, approach doesn't work), prefer reality: solve it, and record the deviation in `TASK_DIR/step04-execution.md` (what differed, what you did instead).
5. **Business-logic questions** → `QUESTIONS_JSON` protocol (same as analyze-task: one line `QUESTIONS_JSON: [...]`, then end the turn). Technical decisions are yours — never ask about those.
6. Follow the project's CLAUDE.md rules and the style of surrounding code.
7. Verify the result works (run the affected flow/tests where possible) before finishing.
8. Squash all WIP commits into one:
   `git reset --soft "$BASE" && git commit -m "<REFERENCE>: <TITLE>"`
   Commit title = the sentence from TITLE, capitalized, no hyphen-slug formatting.
9. Write `TASK_DIR/step04-execution.md` — a short record of what was implemented, deviations from the plan, how it was verified, and the resulting commit hash.
10. Update `TASK_DIR/task.md` (compact living summary): set current stage = "выполнение", fill "Что сделано" (brief) and the commit hash. Keep it brief — latest state only.
11. **Never push. Never create merge requests.** The pipeline ends with local commits.

## Output

Final message: what was implemented, deviations from the plan (if any), how it was verified, resulting commit hash.
