---
name: handoff-notes
description: Write the hand-off instructions for a finished task branch — how to run, install, test or otherwise verify the result on this machine, in this workspace, for this project. Last pipeline stage, after review-task. Produces step06-handoff.md in Russian.
---

# Hand-off notes

You are the last stage. The code is written, reviewed and committed. The person now has to *see* the result — and the most common failure at this point is a stale build: an old binary, an old bundle, an embedded `dist/` from before the change, a schema without the new migration. Your job is to write instructions that make that failure impossible.

**Before anything else, read `TASK_DIR/task.md`** — the compact living progress summary — for the latest state of the solution.

## Inputs (provided in the prompt)

- `TASK_DIR` — contains the requirements (`step01-import.md` if import ran), the plan (`step03-refined-plan.md` or `step02-analyze.md`), `step04-execution.md` and `step05-review.md` (if review ran)
- `REFERENCE`, `BRANCH`, `BASE_COMMIT` — the task's reference, its branch and the base SHA
- `WORKSPACE` — `folder` (the branch is checked out right in the project folder) or `worktree` (a separate worktree at CWD; the project folder still has its own branch)
- `WORKTREE_DIR` — absolute path of the checkout with the result
- `TEST_CMD` — the project's test command, if configured (empty otherwise)
- CWD — the task's checkout. Read-only: you must NOT change code, commit, or build artifacts into the repository.

## Process

1. Read the diff summary (`git diff --stat <BASE_COMMIT>...HEAD`) and `step04-execution.md`: what exactly changed — backend, frontend, config, migrations, dependencies, generated files.
2. Find out how this project is built and run. Sources, in order: `README*`, `CLAUDE.md`, `Makefile`/`Taskfile`/`justfile`, `package.json` scripts, `go.mod`, `Dockerfile`/`docker-compose*`, CI config. Quote real commands from them — do not invent a build system.
3. Identify **everything that must be rebuilt or re-applied** for the change to be visible, and say so explicitly:
   - compiled binaries (Go, Rust, Java…): the running binary on the machine is *old* until rebuilt;
   - frontend bundles, especially ones embedded into a backend (`go:embed`, static folders copied at build time): rebuild the frontend **before** the backend;
   - database migrations, seed data, config/env keys added by the change;
   - dependency installs (`npm install`, `go mod download`, `pip install`) when lockfiles changed;
   - generated code (protobuf, ORM models) when sources changed.
4. Explain where the result lives given `WORKSPACE`: in `folder` mode the project folder *is* the branch — but a build started before the task finished is stale; in `worktree` mode the person must either run from `WORKTREE_DIR` or merge/check out `BRANCH` in the project folder first.
5. Write `TASK_DIR/step06-handoff.md` **in Russian** (technical identifiers and commands as-is) with these sections:
   - **Где результат** — branch, checkout path, workspace mode consequence (one or two sentences).
   - **Собрать и запустить** — an ordered list of concrete shell commands in the right order, each with a one-line reason when it is not obvious (e.g. «фронтенд встраивается в бинарник через go:embed — сначала он, потом go build»). Include the URL/port/entry point to open.
   - **Что проверить руками** — a checklist mapped to the task requirements: what to click/call and what the expected result is; include a negative case where the change has one.
   - **Тесты** — `TEST_CMD` if set, plus the project's own test commands; say what they cover and what they do not (e.g. «фронтендовых автотестов в проекте нет»).
   - **Если что-то не так** — the 2–3 most likely reasons the change is *not visible* (stale binary, cached bundle, old data) and how to tell.
   - **Дальше** — what is left to do outside the code: merge into the base branch, push, MR/PR, deploy, migrations on other environments, documentation — only what applies.
   Keep it short: a person should be able to follow it top to bottom in a few minutes. No praise, no summary of the implementation — that is in `step04-execution.md`.
6. Update `TASK_DIR/task.md`: current stage = «инструкция по проверке», next step = the first command of «Собрать и запустить».

## Output

Final message: the «Собрать и запустить» commands and the «Что проверить руками» checklist, in Russian — the same content as in step06-handoff.md, without the other sections.
