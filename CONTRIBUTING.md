# Contributing

How we work with Git and GitLab on this project. The goal: keep `main` always working
and demoable, and make every change visible and reviewable.

**New to Git / Scrum? Read this whole page once.** There's a plain-English glossary at the
bottom. The workflow below is the same for everyone, every time — follow the steps in order.

---

## The workflow at a glance

Every piece of work follows the same 7 steps:

1. **Pick or create an issue** for the work.
2. **Create a branch + a draft merge request _before_ you start coding.**
3. **Do the work** — commit and push little and often.
4. **Keep the MR description updated** with what you did and why.
5. **Mark the MR ready and request a reviewer** when it's done.
6. **Address review comments.**
7. **Merge** once approved and CI is green.

> **Golden rule: no work without an issue, and no work without an MR open.**
>
> Issues + MRs are how the project stays **visible and trackable**. Together they create a
> clear trail of *who did what, when, and why* — which matters for three reasons:
> - **Review:** work in an MR can be read, discussed, and checked before it reaches `main`.
> - **Marking:** our demo mark depends on the demi being able to *see* the project's progress
>   and history — a well-tracked board and clean MR trail is direct evidence of good process.
> - **Individual peer review:** marks are moderated per person, and GitLab's record of your
>   issues, commits, and MRs is the fairest evidence of your contribution. Work done off the
>   board is invisible — it can't be reviewed, and it won't count for you.
>
> If it isn't an issue on the board and a change in an MR, as far as the team, the reviewer,
> and the marker are concerned, **it didn't happen.**

---

## Step 1 — Pick or create an issue

An **issue** is one unit of work (a feature, a fix, a task). Everything we do starts as an
issue on the GitLab **board**.

- Go to the repo → **Plan → Issues** (or the **Board**).
- **Claim** an existing issue by assigning it to yourself, or **create a new one** if the
  work isn't captured yet. Give it a clear title and a short description of what "done" means.
- Move its card to **In Progress** on the board so everyone knows you're on it.
- **Only work on one issue at a time.** Finish it before starting the next.

## Step 2 — Create a branch + draft MR *before* you start

We open the merge request **first, empty**, so your work is visible from the very beginning.
This is the easiest and safest way — GitLab does the fiddly Git bits for you.

**Easiest way (from the issue page):**
1. Open your issue.
2. Click "Create merge request", which makes both a branch *and* a draft MR.
3. GitLab creates a branch named after the issue (e.g. `12-connection-logging`) and a **Draft**
   MR linked to it. Done — the MR now exists before you've written any code.

**Then get that branch onto your computer:**
```bash
git checkout main
git pull                       # get the latest main
git fetch                      # see the new branch GitLab made
git checkout <branch-name>     # switch onto your branch, e.g. 12-connection-logging
```

**If you'd rather do it from the terminal:** create the branch yourself, push it, then open
a Draft MR in GitLab:
```bash
git checkout main
git pull
git checkout -b feat/12-connection-logging   # see naming below
git push -u origin feat/12-connection-logging
# then in GitLab: Merge requests → New → set your branch → tick "Mark as draft" → Create
```

> **Why open the MR before starting?** It makes your work visible immediately, runs CI on
> every push, gives teammates a place to comment early, and means there's never a surprise
> "giant MR" at the end. A **Draft** MR can't be merged by accident.

## Step 3 — Do the work (commit and push often)

Work on your branch. Save your progress in small, logical **commits**, and **push** so it
shows up in the MR:
```bash
git add .
git commit -m "Add connection-level audit logging"
git push
```
- Commit in small chunks with clear, present-tense messages — not one giant commit at the end.
- Push regularly. Every push updates the MR and re-runs CI, so you catch problems early.
- Pull `main` into your branch now and then to avoid drifting too far:
  ```bash
  git checkout main && git pull
  git checkout <your-branch> && git merge main
  ```

## Step 4 — Keep the MR description updated

The MR **description** is how the team (and your reviewer) understands your work without
reading every line. Keep it current as you go. Use this template:

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
- Auth-failure logging not done yet — will be a separate issue
```

Fill in **what & why**, **what changed**, **how to test it**, and **anything relevant**
(decisions, things you're unsure about, follow-ups). `Closes #12` auto-links the issue so it
closes when the MR merges.

## Step 5 — Mark ready & request a reviewer

When the work is finished and CI is green:
1. **Remove the Draft status** (click **"Mark as ready"** on the MR).
2. **Assign a Reviewer** — on the MR, set the **Reviewer** field to a teammate.
3. **Actually tell them.** Assigning isn't enough — **message the person** in the team chat
4. Move the board card to **In Review**.

## Step 6 — Address the review

Your reviewer will leave comments. This is normal and good — it's how we catch bugs and
learn from each other (**code review is also graded**).
- Reply to comments, push fixes (the MR updates automatically).
- Resolve threads once handled. Re-request review if there were big changes.

## Step 7 — Merge

Once you have **at least one approval** and **CI is green**, merge the MR (**"Merge"** button).
The issue closes and the board card moves to **Done**. `main` stays working. 🎉

> **Do NOT delete the branch when merging.** When GitLab asks, **untick "Delete source
> branch"**. We keep every merged branch on purpose — it preserves a visible record of each
> piece of work and the workflow behind it, which is direct evidence for review and marking.
> Keeping branches costs us nothing; deleting them loses that history.

---

## Reference

### Branch naming
If GitLab named the branch from the issue (e.g. `12-connection-logging`), that's fine to keep.
If you name it yourself, use a prefix:
```
feat/<issue#>-short-slug     # new feature      e.g. feat/12-connection-logging
fix/<issue#>-short-slug      # bug fix          e.g. fix/18-handshake-timeout
chore/<issue#>-short-slug    # tooling/docs     e.g. chore/3-ci-setup
spike/<issue#>-short-slug    # throwaway spike  e.g. spike/6-go-mysql
```

### Branching model
Simple GitLab flow — a single stable trunk plus short-lived branches:
- **`main`** is the single source of truth. It is **protected** and always working/demoable.
  **Never push straight to `main`** — all changes go through an MR.
- One short-lived branch per issue; merge it back within days, not weeks.
- **Keep merged branches — never delete them.** They're our record of the work and workflow
  for review and marking (untick "Delete source branch" when merging).
- No permanent `staging`/`develop` branch (not worth the overhead for our size). We'll add a
  single `develop` branch only if half-done work starts breaking `main` between demos.

### CI
- `.gitlab-ci.yml` runs build + lint + tests on every push and MR.
- A **red pipeline blocks merge.** Fix it, or keep the MR as Draft until it's green.

### Tagging demos
At each sprint demo we tag the commit so we always have a known-good point to return to:
```bash
git tag sprint-1-demo
git push origin sprint-1-demo
```

### Definition of Done
A task is done when: code is written, **unit-tested**, **reviewed and merged** via MR,
**CI is green**, and it **runs in `docker-compose`**.

---

## Glossary (for anyone new to Git/Scrum)

- **Issue** — one unit of work, tracked on the board. Everything starts here.
- **Branch** — your own copy of the code to work on, so you don't disturb `main`.
- **Commit** — a saved snapshot of your changes, with a message describing them.
- **Push** — upload your commits to GitLab so others (and CI) can see them.
- **Merge Request (MR)** — a request to merge your branch into `main`; where review and CI
  happen. (GitHub calls this a "Pull Request".)
- **Draft MR** — an MR still in progress; can't be merged until marked "ready".
- **Reviewer** — a teammate who reads your MR and approves or requests changes.
- **CI (pipeline)** — automated build + tests that run on every push.
- **`main`** — the stable branch we demo from; protected, always working.
- **Sprint** — a fixed work period ending in a demo to the client. We run three.
