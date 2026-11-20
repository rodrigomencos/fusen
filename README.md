# fusen

A board of sticky notes that lives in your project, kept by you and your agents.
*Fusen* (付箋) is Japanese for a sticky note.

![The fusen canvas: notes, a checklist, tasks held by agents, and one question waiting on the person](docs/canvas.png)

## Why use it

| Without it | With fusen |
|---|---|
| Every new agent session rediscovers how the project works. | Decisions, how-tos and open questions sit in notes that the next session reads first. |
| Two agents pick up the same piece of work. | An agent claims a task before starting. Of two claims made at the same moment, one wins and the other is told who holds it. |
| An agent dies halfway and its work is stuck as "in progress". | A claim is a 15-minute lease. If it is not renewed, the task goes back to the queue on its own. |
| A stuck agent guesses, or stops without telling you. | It parks the task on a question and waits. The note shows "needs you" on your canvas, and your reply hands the task back. |
| You can't tell what your agents are doing right now. | One canvas shows every note and task, who holds what, and what is waiting on you. It updates within half a second of an agent writing. |
| You have to ask each agent what changed while you were away. | `fusen log --new` lists what changed since you, or that agent, last looked. |
| Shared context lives in a hosted tool, away from the code. | Notes are Markdown files in `.fusen/notes/`. They diff in git and travel with the repo. |
| Each agent tool needs its own integration. | Anything that can run a command or edit a file can use it. One skill teaches Claude Code and Codex the habits. |

It is a single binary. Nothing is hosted and there is no account: the canvas is
served from your own machine, in the browser (`fusen`) or the terminal
(`fusen tui`).

## Install

```sh
go install github.com/rodrigomencos/fusen@latest
```

Needs Go 1.26 or later.

## Use

```sh
cd your-project
fusen            # the canvas, in the browser
fusen tui        # the canvas, in the terminal
```

Either one creates `.fusen/` on first run. On the canvas, double-click empty
space for a new note and double-click a note to edit it.

From the command line:

```sh
fusen add "Deploy to staging" "make deploy ENV=staging" --label how-to
fusen ls
fusen read deploy-to-staging
fusen edit deploy-to-staging "Check /healthz after." --append
fusen edit deploy-to-staging        # opens it in $EDITOR
```

A note is optional front matter followed by Markdown:

```markdown
---
title: Release checklist
by: claude
labels: release
color: yellow
---

- [x] Tag the build
- [ ] Smoke test checkout
```

## Agents

Install the skill once, and agents learn to keep a board in any project that
has one:

```sh
fusen skill setup     # Claude Code and Codex
fusen skill show      # the guide they follow
```

Give each agent its own name so claims and mentions can tell them apart:

```sh
export FUSEN_AS=claude
```

### Tasks

```sh
fusen add "Rate limit the checkout API" "Done when the tests pass." --task
fusen add "Review the rate limiter" --task --needs rate-limit-the-checkout-api --for codex

fusen claim                  # take the oldest ready task
fusen renew <id>             # a claim lasts 15 minutes
fusen done <id> "what was done"
fusen tasks                  # open tasks and who holds them
```

```
ID                           STATE                                   WHO     TITLE
cap-on-collected-perks       blocked: needs_input                    claude  Cap on collected perks
rate-limit-the-checkout-api  claimed until 13:25                     claude  Rate limit the checkout API
review-the-rate-limiter      waiting on rate-limit-the-checkout-api  codex   Review the rate limiter
```

Two agents claiming the same task at the same moment get one winner. The other
is told who holds it and until when.

`fusen run <id> -- <command>` claims a task, runs the command, keeps the claim
alive while it runs, and settles the task by the command's exit code.

### Questions and mentions

```sh
fusen block <id> "Cap at 50 or 100? I recommend 50."   # asks the person
fusen wait <id>                                        # returns when they answer
```

The question lands on the note, signed and dated, and the person sees it marked
"needs you" with a box to answer in:

```
$ fusen inbox
cap-on-collected-perks  Cap on collected perks
    @alex Cap at 50 or 100 per crew? I recommend 50: it matches the badge limit.
```

Writing `@name` in any note or reply does the same for that person or agent,
until they reply.

### What changed

```sh
fusen log --new      # since you last looked
fusen tail           # as it happens
```

Notes edited by hand are logged too: the log is built by comparing the notes
with what they were, not by the commands that change them.

## Checklists from wedo

A note can show a [wedo](https://github.com/rodrigomencos/wedo) checklist under its
text, live, with `wedo: <list>` in its front matter or `--wedo <list>`. This is
optional: plain `- [ ]` lines in a note need nothing else.

## The terminal canvas

`fusen tui` draws each card as an image under the text, using the kitty graphics
protocol, in terminals that support it (Ghostty and kitty among them). Elsewhere
it falls back to cards made of colored cells. `fusen tui --graphics off` forces
the fallback.

## What is in `.fusen/`

| Path | What it is | Commit it? |
|---|---|---|
| `notes/*.md` | The notes | Yes |
| `layout.json` | Where notes sit on the canvas | Your choice |
| `README.md` | The format, for agents reading the directory | Yes |
| `.local/` | This machine's change log, bookmarks and lock | No (ignored) |

## Status

Early. The commands and file format may still change.
