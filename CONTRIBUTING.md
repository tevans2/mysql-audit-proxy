# Contributing

How we work with Git and GitHub on this project. The goal: keep `main` always working
and demoable, and make every change visible and reviewable.

**New to Git or pull requests? Read this page once.** There is a plain-English glossary at
the bottom. The workflow is the same for everyone, every time.

---

## The workflow at a glance

Every piece of work follows the same 7 steps:

1. **Pick or create an issue** for the work.
2. **Create a branch and open a draft pull request _before_ you start coding.**
3. **Do the work**, committing and pushing little and often.
4. **Keep the pull request description updated** with what you did and why.
5. **Mark the pull request ready and request a reviewer** when it's done.
6. **Address review comments.**
7. **Merge** once it is approved and CI is green.

> **Golden rule: no work without an issue, and no work without a pull request open.**
>
> Issues and pull requests are how the project stays **visible and trackable**. Together they
> leave a clear trail of *who did what, when, and why*:
> - **Review:** work in a pull request can be read, discussed and checked before it reaches `main`.
> - **History:** the issue explains the reason for a change long after the commit is forgotten.
> - **Coordination:** the board shows what is in progress, so two people don't do the same thing.

---

## Step 1 — Pick or create an issue

An **issue** is one unit of work (a feature, a fix, a task). Everything starts as an issue on
the project board.

- Go to the repository → **Issues** (or the **Projects** board).
- **Claim** an existing issue by assigning it to yourself, or **create a new one** if the work
  isn't captured yet. Give it a clear title and a short description of what "done" means.
- Move its card to **In Progress** on the board.
- **Work on one issue at a time.** Finish it before starting the next.

## Step 2 — Create a branch and a draft pull request *before* you start

Open the pull request **first, nearly empty**, so your work is visible from the beginning.

**Easiest way (from the issue page):**
1. Open your issue.
2. In the right-hand sidebar, under **Development**, click **Create a branch**.
3. GitHub creates a branch named after the issue (e.g. `12-connection-logging`).

**Then get that branch onto your computer:**
```bash
git checkout main
git pull                       # get the latest main
git fetch                      # see the new branch GitHub made
git checkout <branch-name>     # switch onto your branch, e.g. 12-connection-logging
```

**Then open the draft pull request.** A branch with no commits can't have a pull request, so
make a first small commit (even a note in the description file), push it, and:
```bash
git commit --allow-empty -m "Start work on #12"
git push -u origin <branch-name>
gh pr create --draft --fill     # or: Pull requests → New pull request → "Create draft pull request"
```

**If you'd rather create the branch from the terminal:**
```bash
git checkout main
git pull
git checkout -b feat/12-connection-logging   # see naming below
git commit --allow-empty -m "Start work on #12"
git push -u origin feat/12-connection-logging
gh pr create --draft --fill
```

> **Why open the pull request before starting?** It makes your work visible immediately, runs
> CI on every push, gives teammates a place to comment early, and means there is never a
> surprise "giant pull request" at the end. A **draft** pull request can't be merged by accident.

## Step 3 — Do the work (commit and push often)

Work on your branch. Save your progress in small, logical **commits**, and **push** so it shows
up in the pull request:
```bash
git add .
git commit -m "Add connection-level audit logging"
git push
```
- Commit in small chunks with clear, present-tense messages, not one giant commit at the end.
- Push regularly. Every push updates the pull request and re-runs CI, so you catch problems early.
- Pull `main` into your branch now and then to avoid drifting too far:
  ```bash
  git checkout main && git pull
  git checkout <your-branch> && git merge main
  ```

## Step 4 — Keep the pull request description updated

The **description** is how the team understands your work without reading every line. Keep it
current as you go. Use this template:

```markdown
## What & why
Closes #12. Adds connection-level audit logging so we capture who connected, when,
and to which upstream.

## What changed
- New `audit` package that writes a record on connect/disconnect
- Added a `connections` table + migration
- Wired the proxy session handler to emit events

## How to test / see it working
- `docker-compose up`, connect a MySQL client, then check the `connections` table

## Notes / open questions
- Auth-failure logging not done yet; that will be a separate issue
```

Fill in **what & why**, **what changed**, **how to test it**, and **anything relevant**
(decisions, things you're unsure about, follow-ups). `Closes #12` links the issue, so it closes
automatically when the pull request merges.

## Step 5 — Mark ready and request a reviewer

When the work is finished and CI is green:
1. Click **Ready for review** on the pull request to remove the draft status.
2. **Request a review** from a teammate, using the **Reviewers** field in the sidebar.
3. **Actually tell them.** Requesting isn't enough; message the person.
4. Move the board card to **In Review**.

## Step 6 — Address the review

Your reviewer will leave comments. This is normal and good: it is how we catch bugs and learn
from each other.
- Reply to comments and push fixes (the pull request updates automatically).
- Resolve conversations once they are handled. Re-request review if there were big changes.

## Step 7 — Merge

Once you have **at least one approval** and **CI is green**, merge the pull request with the
**Merge pull request** button. The issue closes and the board card moves to **Done**. `main`
stays working.

> **Keep merged branches.** When GitHub offers **Delete branch** after the merge, leave it
> alone unless the team has agreed otherwise. A kept branch preserves a visible record of each
> piece of work. (Under **Settings → General → Pull Requests**, make sure *Automatically
> delete head branches* is off if you want this to be the default.)

---

## Reference

### Branch naming
If GitHub named the branch from the issue (e.g. `12-connection-logging`), that's fine to keep.
If you name it yourself, use a prefix:
```
feat/<issue#>-short-slug     # new feature      e.g. feat/12-connection-logging
fix/<issue#>-short-slug      # bug fix          e.g. fix/18-handshake-timeout
chore/<issue#>-short-slug    # tooling/docs     e.g. chore/3-ci-setup
spike/<issue#>-short-slug    # throwaway spike  e.g. spike/6-go-mysql
```

### Branching model
Trunk-based: a single stable trunk plus short-lived branches.
- **`main`** is the single source of truth. Protect it under **Settings → Branches → Branch
  protection rules** (require a pull request, one approval and passing CI). It is always
  working and demoable. **Never push straight to `main`.**
- One short-lived branch per issue; merge it back within days, not weeks.
- **Keep merged branches.** They are the record of the work.
- No permanent `staging` or `develop` branch (not worth the overhead for our size). Add a
  single `develop` branch only if half-done work starts breaking `main` between demos.

### CI
- `.github/workflows/ci.yml` runs lint, unit tests, end-to-end tests and the web checks on every
  pull request and every push to `main`.
- A **red check blocks merge** once branch protection requires it. Fix it, or keep the pull
  request as a draft until it is green.
- To run the same checks locally, see the `Makefile` (`make lint`, `make test`, `make web-test`,
  `make test-e2e`).

### Tagging demos
At each sprint demo we tag the commit so we always have a known-good point to return to:
```bash
git tag sprint-1-demo
git push origin sprint-1-demo
```

### Definition of Done
A task is done when: code is written, **unit-tested**, **reviewed and merged** through a pull
request, **CI is green**, and it **runs in `docker-compose`**.

---

## Glossary (for anyone new to Git or Scrum)

- **Issue**: one unit of work, tracked on the board. Everything starts here.
- **Branch**: your own copy of the code to work on, so you don't disturb `main`.
- **Commit**: a saved snapshot of your changes, with a message describing them.
- **Push**: upload your commits to GitHub so others (and CI) can see them.
- **Pull request (PR)**: a request to merge your branch into `main`; where review and CI happen.
- **Draft pull request**: a pull request still in progress; it can't be merged until marked "ready".
- **Reviewer**: a teammate who reads your pull request and approves or requests changes.
- **CI (GitHub Actions)**: automated build and tests that run on every push and pull request.
- **`main`**: the stable branch we demo from; protected, always working.
- **Sprint**: a fixed work period ending in a demo to the client. We run three.
