---
name: review-task
description: Review a completed task branch — the full diff against the base branch, checked against the task requirements and the solution plan; report written in Russian; optionally auto-fix confirmed findings. Use after execute-plan.
---

# Review task

**Before anything else, read `TASK_DIR/task.md`** — the compact living progress summary — for the latest state of the solution.

## Inputs (provided in the prompt)

- `TASK_DIR` — contains `step01-import.md` (requirements), the plan (`step03-refined-plan.md` if it exists, else `step02-analyze.md`), and `step04-execution.md`
- `REFERENCE` — e.g. `tn/core/tradernet#42546`
- `BASE_COMMIT` — the base SHA recorded by the orchestrator at worktree creation; the diff is computed against it (the local base branch in the worktree may be stale — do not diff against a branch name)
- `MODE` — `report-only` | `autofix`
- CWD — the task's git worktree

## Process

1. Read the requirements and the plan. The diff is judged against BOTH: does it solve the task, and did it silently skip plan items (check `step04-execution.md` for recorded deviations — those are legitimate).
2. Review the full branch diff. The prompt may already contain it in a `<<<DIFF ... DIFF>>>` section — use that instead of running git diff (fall back to `git diff <BASE_COMMIT>...HEAD` only if the section is absent or patches were omitted for size). Look for:
   - correctness bugs (logic, types, edge cases, SQL/PG pitfalls);
   - requirement mismatches and missed plan items;
   - regressions in touched code;
   - violations of project rules (CLAUDE.md: migrations naming/location, SQL type comparisons, caching rules, docblock style, …).
3. Verify each finding against the actual code before reporting — reject false positives; a finding must have a concrete failure scenario or rule citation.
4. Write `TASK_DIR/step05-review.md` **in Russian** — this is a hard user rule even though this skill is written in English: обзор, находки по severity (критично / важно / незначительно) с указанием файла и строки, итог. Technical identifiers and code blocks stay as-is.
5. Findings handling by MODE:
   - `autofix`: fix confirmed findings, commit each fix as `<REFERENCE>: <короткое описание правки>` (commit messages follow the same reference convention). Ambiguous or debatable findings go to the report only — do not guess.
   - `report-only`: change nothing; report only.
6. Update `TASK_DIR/task.md` (compact living summary): set current stage = "ревью", record the verdict (готово / есть замечания) and remaining open items. Keep it brief — latest state only.

## Output

Final message: the review summary in Russian (same content as step05-review.md's overview + findings list), and in autofix mode — what was fixed and the commit hashes.
