package main

import (
	"cmp"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// A note becomes a task by carrying a task state in its front matter:
//
//	task: claimed
//	claim: claude until 2026-11-20T14:20:00Z
//	needs: write-migration
//	for: codex
//
// An agent claims a task before working on it. The claim is a lease: if the
// agent goes away without settling the task, the lease runs out and the task
// is ready for someone else. Nothing has to run for that to happen, because
// the state is worked out from the clock whenever the task is read.

const defaultLease = 15 * time.Minute

// taskState is what a task is right now: the state in its file, except that a
// lapsed claim makes it ready again and unfinished prerequisites make it wait.
func taskState(n Note, notes []Note) string {
	switch n.Task {
	case "", "blocked", "done", "failed":
		return n.Task
	case "claimed":
		if time.Now().Before(n.Until) {
			return "claimed"
		}
	}
	for _, need := range n.Needs {
		if i := slices.IndexFunc(notes, func(o Note) bool { return o.ID == need }); i < 0 || notes[i].Task != "done" {
			return "waiting"
		}
	}
	return "ready"
}

// locked runs fn while holding the board's lock, so two agents changing the
// board at the same moment take turns.
func (s Store) locked(fn func() error) error {
	if err := os.MkdirAll(s.local(""), 0o755); err != nil {
		return err
	}
	// What is under .local describes this machine's view of the board, so it stays out of git.
	if ignore := s.dir + "/.gitignore"; !exists(ignore) {
		os.WriteFile(ignore, []byte(".local/\n"), 0o644)
	}
	f, err := os.OpenFile(s.local("lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	return fn()
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// change rewrites one note under the lock. fn also gets every note, for
// rules that depend on other tasks.
func (s Store) change(id string, fn func(n *Note, notes []Note) error) (Note, error) {
	var saved Note
	err := s.locked(func() error {
		notes, err := s.List()
		if err != nil {
			return err
		}
		i := slices.IndexFunc(notes, func(n Note) bool { return n.ID == id })
		if i < 0 {
			return fmt.Errorf("no note %q: %w", id, os.ErrNotExist)
		}
		n := notes[i]
		if err := fn(&n, notes); err != nil {
			return err
		}
		saved, err = s.Save(n)
		return err
	})
	return saved, err
}

// Errors from task commands start with a short reason an agent can match on.

func claim(n *Note, notes []Note, who string, lease time.Duration) error {
	switch state := taskState(*n, notes); {
	case state == "":
		return fmt.Errorf("not_a_task: %s is a plain note (fusen reopen %s makes it a task)", n.ID, n.ID)
	case state == "claimed" && !strings.EqualFold(n.Holder, who):
		return fmt.Errorf("task_claimed: %s holds %s until %s", n.Holder, n.ID, n.Until.Local().Format("15:04"))
	case state == "waiting":
		return fmt.Errorf("task_waiting: %s needs %s finished first", n.ID, strings.Join(n.Needs, ", "))
	case state != "ready" && state != "claimed":
		return fmt.Errorf("task_%s: %s cannot be claimed", state, n.ID)
	case n.For != "" && !strings.EqualFold(n.For, who):
		return fmt.Errorf("task_assigned: %s is for %s", n.ID, n.For)
	}
	n.Task, n.Holder, n.Until, n.By = "claimed", who, time.Now().Add(lease), who
	return nil
}

// claimNext claims the oldest task who may take: those assigned to who
// first, and only those carrying every label.
func (s Store) claimNext(who string, lease time.Duration, labels []string) (Note, error) {
	var saved Note
	err := s.locked(func() error {
		notes, err := s.List()
		if err != nil {
			return err
		}
		queue := slices.Clone(notes)
		slices.SortStableFunc(queue, func(a, b Note) int {
			if a.For != b.For {
				return cmp.Compare(b.For, a.For) // assigned before unassigned
			}
			return a.age().Compare(b.age())
		})
		for _, n := range queue {
			if taskState(n, notes) != "ready" || slices.ContainsFunc(labels, func(l string) bool { return !slices.Contains(n.Labels, l) }) {
				continue
			}
			if claim(&n, notes, who, lease) == nil {
				saved, err = s.Save(n)
				return err
			}
		}
		return fmt.Errorf("nothing_ready: no task is ready for %s", who)
	})
	return saved, err
}

// age orders tasks in the queue: by when they were made, or last written when that is unknown.
func (n Note) age() time.Time {
	if n.Created.IsZero() {
		return n.Updated
	}
	return n.Created
}

// held checks that who may settle a task: it is a task and nobody else has a live claim on it.
func held(n *Note, notes []Note, who string) error {
	if n.Task == "" {
		return fmt.Errorf("not_a_task: %s is a plain note", n.ID)
	}
	if taskState(*n, notes) == "claimed" && !strings.EqualFold(n.Holder, who) {
		return fmt.Errorf("claim_lost: %s holds %s now, stop working on it", n.Holder, n.ID)
	}
	return nil
}

func renew(n *Note, who string, lease time.Duration) error {
	if n.Task != "claimed" || !strings.EqualFold(n.Holder, who) {
		return fmt.Errorf("claim_lost: %s does not hold %s, stop working on it", who, n.ID)
	}
	n.Until = time.Now().Add(lease)
	return nil
}

// settle moves a task to a state nobody holds: ready, done or failed.
func settle(n *Note, who, state string) {
	n.Task, n.Holder, n.Until, n.Blocked, n.By = state, "", time.Time{}, "", who
	n.Finished = time.Time{}
	if n.finished() {
		n.Finished = time.Now()
	}
}

// finished reports whether a task is settled for good, done or failed, and so
// sits on the board's Done shelf instead of the canvas.
func (n Note) finished() bool { return n.Task == "done" || n.Task == "failed" }

// finishedAt is when a task was settled, or when its file was last written if it does not say.
func (n Note) finishedAt() time.Time {
	if n.Finished.IsZero() {
		return n.Updated
	}
	return n.Finished
}

// block parks a task on a question. The holder keeps it, with no deadline,
// until someone answers.
func block(n *Note, who, kind string) {
	n.Task, n.Holder, n.Until, n.Blocked, n.By = "blocked", who, time.Time{}, kind, who
}

// unblock hands a blocked task back to whoever was holding it, with a fresh lease.
func unblock(n *Note) {
	n.Blocked = ""
	if n.Holder == "" {
		n.Task = "ready"
		return
	}
	n.Task, n.Until = "claimed", time.Now().Add(defaultLease)
}

// ---------- Replies and mentions ----------

// A reply is a paragraph at the end of a note that opens with a line naming who wrote it and when.
var reReply = regexp.MustCompile(`(?m)^\*\*([^*\n]+)\*\* · \d{4}-\d{2}-\d{2} \d{2}:\d{2}$`)

func addReply(n *Note, who, text string) {
	n.Body = trimBody(n.Body) + "\n\n**" + who + "** · " + time.Now().Format("2006-01-02 15:04") + "\n" + strings.TrimSpace(text)
	n.By = who
}

// lastReply is the text of a note's newest reply.
func lastReply(body string) string {
	all := reReply.FindAllStringIndex(body, -1)
	if len(all) == 0 {
		return ""
	}
	return strings.TrimSpace(body[all[len(all)-1][1]:])
}

var (
	reMention    = regexp.MustCompile(`(?:^|[^\w@./-])@([A-Za-z0-9][\w.-]*)`)
	reInlineCode = regexp.MustCompile("`[^`\n]*`")
)

// mentions lists the names written as @name in text, in lower case, leaving out code.
func mentions(text string) []string {
	var prose strings.Builder
	fenced := false
	for _, l := range strings.Split(text, "\n") {
		if reFence.MatchString(l) {
			fenced = !fenced
		} else if !fenced {
			prose.WriteString(reInlineCode.ReplaceAllString(l, "") + "\n")
		}
	}
	var names []string
	for _, m := range reMention.FindAllStringSubmatch(prose.String(), -1) {
		names = append(names, strings.ToLower(strings.TrimRight(m[1], ".-")))
	}
	return names
}

// needsReply reports whether a note is waiting on who: it mentions them
// somewhere after the last reply they wrote.
func needsReply(n Note, who string) bool {
	from := 0
	for _, m := range reReply.FindAllStringSubmatchIndex(n.Body, -1) {
		if strings.EqualFold(n.Body[m[2]:m[3]], who) {
			from = m[1]
		}
	}
	return slices.Contains(mentions(n.Body[from:]), strings.ToLower(who))
}
