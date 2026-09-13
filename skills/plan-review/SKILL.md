---
name: plan-review
description: Independent fresh-context review of a solution plan against the task requirements and the real codebase — find the top ~8 errors/risks/omissions, verify each against code, write the corrected plan to step03-refined-plan.md. One pass per invocation; the orchestrator runs it N times for N passes.
---

# Plan review (error-work pass)

You are an independent reviewer with a clean context: you did NOT write this plan. Trust nothing in it — every claim must survive contact with the actual code.

**Before anything else, read `TASK_DIR/task.md`** — the compact living progress summary — to understand the latest state (useful if a run was interrupted or the user added feedback).

## Inputs (provided in the prompt)

- `TASK_DIR` — contains `step01-import.md` (requirements) and the plan: `step02-analyze.md` (from the analysis stage). On pass 2+ a `step03-refined-plan.md` from the previous pass already exists — refine that one.
- `PASS_NUMBER` — which pass this is (1..N)
- CWD — the task's git worktree (checked out from the fresh base branch). Read-only, except `TASK_DIR/step03-refined-plan.md` (the file you produce).

## Output artifact

This stage produces `TASK_DIR/step03-refined-plan.md` — the corrected plan. On pass 1, start from `step02-analyze.md` (copy its content, then apply fixes). On later passes, refine the existing `step03-refined-plan.md`. Never modify `step02-analyze.md` (the original analysis is preserved).

## Pass scope (saves tokens — follow it)

- **Pass 1** — full review: verify the whole plan against the requirements and the codebase.
- **Pass 2+** — delta review, NOT a full redo. Read the `Self-review` section of `step03-refined-plan.md` first, then verify only:
  1. the fixes applied by the previous pass (did they actually land, are they correct against the code);
  2. the plan sections the previous pass modified;
  3. any requirement the Self-review shows was never verified yet.
  Claims already confirmed by earlier passes are NOT re-verified. If the previous pass ended `PLAN_REVIEW: clean`, you will not be called — no need to handle that case.

## Process

1. Read the requirements first (`step01-import.md` + attachments if referenced), then the plan. The prompt may already contain the current plan text in a `<<<PLAN ... PLAN>>>` section — use it instead of reading the plan file (your edits still go to `step03-refined-plan.md`).
2. Check the plan against the requirements: does it actually solve the task? Anything missing, anything invented that wasn't asked for?
3. Verify every factual claim against the real codebase: each listed file exists and does what the plan says it does; each claimed dependency is real; the steps are implementable in the stated order. Cite evidence as `file:line`.
4. Compile the top ~8 errors, risks, or omissions (fewer if fewer genuinely exist — do not pad). Each finding: the plan section/claim + the contradicting evidence.
5. Re-verify each finding against the code before acting — findings themselves can be wrong. Reject false ones.
6. Apply fixes for confirmed findings in `step03-refined-plan.md` (create it from `step02-analyze.md` on pass 1; refine it on later passes) — edit inline, don't just note them.
7. Append to `step03-refined-plan.md`'s `Self-review` section:

```markdown
### Pass <PASS_NUMBER> (plan-review)
- Confirmed & fixed: <finding> → <what was changed>
- Rejected: <finding> → <why it was wrong>
```

If nothing real was found, append `### Pass <N>: no findings`.

8. Update `TASK_DIR/task.md` (compact living summary): set current stage = "работа над ошибками (проход N)" and reflect any plan changes in the short plan. Keep it brief — latest state only.

9. End your final chat message with two verdict markers, each on its own line:

   - `PLAN_REVIEW: clean` — no finding survived verification (nothing was changed in the plan). The orchestrator then skips the remaining passes.
   - `PLAN_REVIEW: revised` — at least one confirmed finding was fixed in the plan.

   Be honest: `clean` on a pass that actually changed the plan skips reviews the plan still needs.

   - `PLAN_RESULT: code` — the refined plan requires changing project code.
   - `PLAN_RESULT: no-code` — the refined plan needs no code changes (execute/review will be skipped).

   This may overturn the analysis stage's verdict in either direction — the orchestrator applies the last one.

## Rules

- Only fix real errors and gaps — no gold-plating, no style rewrites of the plan.
- Never modify project files.
- Severity order: wrong/missing requirement > broken dependency/file claim > risky assumption > unclear step.
