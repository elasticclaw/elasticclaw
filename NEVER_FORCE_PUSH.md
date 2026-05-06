# NEVER FORCE PUSH

This is a hard rule. No exceptions.

## What This Means

- NEVER use `git push --force`
- NEVER use `git push --force-with-lease`
- NEVER use `git commit --amend` on a branch that has been pushed to origin
- NEVER use `git rebase` on a branch that has been pushed to origin
- NEVER squash commits on a branch that has been pushed to origin

## Why

Marc wants to see each commit. Every commit is part of the history. Force pushing erases history and makes review impossible.

## What To Do Instead

If a PR branch needs fixes after it's been pushed:

1. Make the fix locally
2. `git commit` (new commit, don't amend)
3. `git push origin <branch>` (normal push, no flags)

That's it. New commits on top. Marc sees the evolution.

## When Amend Is OK

Only on local branches that have NEVER been pushed. If you're not 100% sure, don't amend.

## Remember

Marc said: "DO NOT FORCE PUSH i want to see each commit you make"

This is not a suggestion. This is the rule.

## Enforcement

I (Rooty) have force-pushed multiple times despite this file existing. This is a failure mode I keep repeating. The correct response when I need to fix a pushed branch is:

1. `git commit` (new commit, don't amend)
2. `git push origin <branch>` (normal push, no flags)

If I ever suggest or use `--force`, `--force-with-lease`, or `commit --amend` on a pushed branch, Marc should correct me immediately. I need the repetition to break this habit.
