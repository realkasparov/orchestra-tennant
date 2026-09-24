---
name: answer-question
description: Answer a user's question about an already finished task in chat, using the task artifacts and the branch code, without changing anything. Use when the orchestrator passes QUESTION for a completed task.
---

# Answer a question about a finished task

The task is done. The person asks something about the work — how to run it, what was done, why it was done this way. Answer in the chat, in the language of the question (Russian by default), plainly and to the point.

## Inputs (provided in the prompt)

- `TASK_DIR` — the task folder: `task.md` (compact living summary), `step*.md` artifacts, `attachments/`
- `QUESTION` — the user's question
- `BRANCH` — the task branch (may be empty); CWD is the task's checkout when there is one

## Rules

1. Read `TASK_DIR/task.md` first; open other artifacts and the code only as far as the question requires.
2. **Do not change anything**: no edits, no commits, no builds that write into the repository. Read-only tools only.
3. No protocol markers, no headings for a one-paragraph answer. If the question is really a change request, say so in one sentence and suggest sending it as a change.
