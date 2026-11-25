package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

// Commands for tasks, replies, and following what changes.

func leaseOf(a args) (time.Duration, error) {
	if !a.has("lease") {
		return defaultLease, nil
	}
	d, err := time.ParseDuration(a.get("lease"))
	if err != nil || d < time.Minute {
		return 0, fmt.Errorf("--lease wants a duration of a minute or more, such as 15m")
	}
	return d, nil
}

func duration(a args, flag string, fallback time.Duration) (time.Duration, error) {
	if !a.has(flag) {
		return fallback, nil
	}
	d, err := time.ParseDuration(a.get(flag))
	if err != nil {
		return 0, fmt.Errorf("--%s wants a duration such as 9m", flag)
	}
	return d, nil
}

// answer adds a reply. A reply to a blocked task from anyone but its holder
// is the answer it was waiting for, so it also puts the task back in play.
func answer(n *Note, who, text string) {
	blockedOn := n.Task == "blocked" && !strings.EqualFold(n.Holder, who)
	addReply(n, who, text)
	if blockedOn {
		unblock(n)
	}
}

func cmdTasks(a args) error {
	if err := a.allow("all", "done", "json"); err != nil {
		return err
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	notes, _, err := s.Board(author(""))
	if err != nil {
		return err
	}
	tasks := slices.DeleteFunc(slices.Clone(notes), func(n Note) bool {
		switch {
		case a.has("done"):
			return !n.finished()
		case a.has("all"):
			return n.State == ""
		}
		return n.State == "" || n.finished()
	})
	// Open tasks are listed in the order claim takes them; finished ones latest first.
	slices.SortStableFunc(tasks, func(x, y Note) int { return x.age().Compare(y.age()) })
	if a.has("done") {
		slices.SortStableFunc(tasks, func(x, y Note) int { return y.finishedAt().Compare(x.finishedAt()) })
	}
	if a.has("json") {
		return json.NewEncoder(os.Stdout).Encode(tasks)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATE\tWHO\tTITLE")
	for _, n := range tasks {
		who := n.For
		switch {
		case n.State == "claimed" || n.State == "blocked":
			who = n.Holder
		case n.finished():
			who = n.By // whoever settled it
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", n.ID, stateText(n), who, n.Title)
	}
	return w.Flush()
}

// stateText says what a task is waiting on, where the state alone does not.
func stateText(n Note) string {
	switch n.State {
	case "claimed":
		return "claimed until " + n.Until.Local().Format("15:04")
	case "blocked":
		return "blocked: " + cmp.Or(n.Blocked, "needs_input")
	case "waiting":
		return "waiting on " + strings.Join(n.Needs, ", ")
	case "done", "failed":
		return n.State + " " + ago(n.finishedAt())
	}
	return n.State
}

// sortNotes orders a listing: newest or oldest by when notes were made,
// changed by when they were last written, or title.
func sortNotes(notes []Note, by string) error {
	var order func(a, b Note) int
	switch by {
	case "newest":
		order = func(a, b Note) int { return b.age().Compare(a.age()) }
	case "oldest":
		order = func(a, b Note) int { return a.age().Compare(b.age()) }
	case "changed":
		order = func(a, b Note) int { return b.Updated.Compare(a.Updated) }
	case "title":
		order = func(a, b Note) int {
			return cmp.Compare(strings.ToLower(cmp.Or(a.Title, a.ID)), strings.ToLower(cmp.Or(b.Title, b.ID)))
		}
	default:
		return fmt.Errorf("--sort wants newest, oldest, changed or title")
	}
	slices.SortStableFunc(notes, order)
	return nil
}

func cmdClaim(a args) error {
	if err := a.allow("as", "lease", "label", "wait", "json"); err != nil {
		return err
	}
	if len(a.pos) > 1 {
		return errors.New("usage: fusen claim [<id>]")
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	lease, err := leaseOf(a)
	if err != nil {
		return err
	}
	wait, err := duration(a, "wait", 0)
	if err != nil {
		return err
	}
	n, err := claimOne(s, a, lease, time.Now().Add(wait))
	if err != nil {
		return err
	}
	if a.has("json") {
		return json.NewEncoder(os.Stdout).Encode(n)
	}
	fmt.Printf("claimed %s until %s (fusen renew %s to keep it)\n\n", n.ID, n.Until.Local().Format("15:04"), n.ID)
	_, err = os.Stdout.Write(n.encode())
	return err
}

// claimOne claims the named task, or the oldest ready one, trying until the deadline for one to turn up.
func claimOne(s Store, a args, lease time.Duration, deadline time.Time) (Note, error) {
	who := author(a.get("as"))
	if len(a.pos) == 1 {
		return s.change(a.pos[0], func(n *Note, notes []Note) error { return claim(n, notes, who, lease) })
	}
	for {
		n, err := s.claimNext(who, lease, a.flags["label"])
		if err == nil || !strings.HasPrefix(err.Error(), "nothing_ready") || time.Now().After(deadline) {
			return n, err
		}
		time.Sleep(time.Second)
	}
}

// cmdTask runs the commands that change one task or add to one note.
func cmdTask(verb string, a args) error {
	if err := a.allow("as", "lease", "kind", "ask"); err != nil {
		return err
	}
	needsText := slices.Contains([]string{"fail", "block", "reply"}, verb)
	takesText := needsText || verb == "done"
	if len(a.pos) < 1 || (needsText && len(a.pos) < 2) || len(a.pos) > 2 || (!takesText && len(a.pos) > 1) {
		if takesText {
			return fmt.Errorf("usage: fusen %s <id> <text|->", verb)
		}
		return fmt.Errorf("usage: fusen %s <id>", verb)
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	lease, err := leaseOf(a)
	if err != nil {
		return err
	}
	text := ""
	if len(a.pos) == 2 {
		if text, err = readBody(a.pos[1]); err != nil {
			return err
		}
	}
	who := author(a.get("as"))
	n, err := s.change(a.pos[0], func(n *Note, notes []Note) error {
		switch verb {
		case "reply":
			answer(n, who, text)
			return nil
		case "reopen":
			settle(n, who, "ready")
			return nil
		case "renew":
			return renew(n, who, lease)
		case "unblock":
			if n.Task != "blocked" {
				return fmt.Errorf("not_blocked: %s is not blocked", n.ID)
			}
			unblock(n)
			return nil
		}
		if err := held(n, notes, who); err != nil {
			return err
		}
		switch verb {
		case "release":
			settle(n, who, "ready")
		case "done", "fail":
			if text != "" {
				addReply(n, who, text)
			}
			settle(n, who, map[string]string{"done": "done", "fail": "failed"}[verb])
		case "block":
			// A question nobody is told about is not asked: mention the person unless the text already names someone.
			if ask := cmp.Or(a.get("ask"), os.Getenv("USER")); len(mentions(text)) == 0 && !strings.EqualFold(ask, who) {
				text = "@" + ask + " " + text
			}
			addReply(n, who, text)
			block(n, who, cmp.Or(a.get("kind"), "needs_input"))
		}
		return nil
	})
	if err != nil {
		return err
	}
	switch {
	case verb == "reply":
		fmt.Println("replied on", n.ID)
	case n.Task == "claimed":
		fmt.Printf("%s: claimed by %s until %s\n", n.ID, n.Holder, n.Until.Local().Format("15:04"))
	case verb == "block":
		fmt.Printf("%s: blocked. fusen wait %s returns when someone answers\n", n.ID, n.ID)
	default:
		fmt.Printf("%s: %s\n", n.ID, n.Task)
	}
	return nil
}

// cmdRun lets a command be the work: it claims a task, keeps the claim alive
// while the command runs, and settles the task by how the command ends.
func cmdRun(a args) error {
	if err := a.allow("as", "lease", "label", "result"); err != nil {
		return err
	}
	if len(a.rest) == 0 || len(a.pos) > 1 {
		return errors.New("usage: fusen run [<id>] -- <command>...")
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	lease, err := leaseOf(a)
	if err != nil {
		return err
	}
	n, err := claimOne(s, a, lease, time.Now())
	if err != nil {
		return err
	}
	who := author(a.get("as"))
	cmd := exec.Command(a.rest[0], a.rest[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "FUSEN_TASK="+n.ID, "FUSEN_AS="+who)
	finish := func(state, text string) error {
		_, err := s.change(n.ID, func(n *Note, notes []Note) error {
			if err := held(n, notes, who); err != nil {
				return err
			}
			if text != "" {
				addReply(n, who, text)
			}
			settle(n, who, state)
			return nil
		})
		s.Sync()
		return err
	}
	if err := cmd.Start(); err != nil {
		finish("ready", "")
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	beat := time.NewTicker(lease / 3)
	defer beat.Stop()
	for {
		select {
		case err := <-exited:
			if err == nil {
				return finish("done", a.get("result"))
			}
			code := 1
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() > 0 {
				code = exit.ExitCode()
			}
			finish("failed", fmt.Sprintf("`%s` ended with exit code %d", strings.Join(a.rest, " "), code))
			os.Exit(code)
		case sig := <-signals:
			// Stopped from outside: the work is not done and not failed, so the task goes back.
			cmd.Process.Signal(sig)
			<-exited
			finish("ready", "")
			os.Exit(128 + int(sig.(syscall.Signal)))
		case <-beat.C:
			if _, err := s.change(n.ID, func(n *Note, _ []Note) error { return renew(n, who, lease) }); err != nil {
				cmd.Process.Kill()
				<-exited
				return err
			}
		}
	}
}

func cmdInbox(a args) error {
	if err := a.allow("as", "json"); err != nil {
		return err
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	who := author(a.get("as"))
	notes, _, err := s.Board(who)
	if err != nil {
		return err
	}
	notes = slices.DeleteFunc(notes, func(n Note) bool { return !n.NeedsMe })
	if a.has("json") {
		return json.NewEncoder(os.Stdout).Encode(notes)
	}
	if len(notes) == 0 {
		fmt.Println("nothing is waiting on", who)
	}
	for _, n := range notes {
		text, more := cut(strings.Join(strings.Fields(cmp.Or(lastReply(n.Body), n.Body)), " "), 160)
		if more != "" {
			text += "…"
		}
		fmt.Printf("%s  %s\n    %s\n", n.ID, cmp.Or(n.Title, n.ID), text)
	}
	return nil
}

func cmdLog(a args) error {
	if err := a.allow("as", "after", "new", "limit", "all", "json"); err != nil {
		return err
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	if err := s.Sync(); err != nil {
		return err
	}
	who := author(a.get("as"))
	limit, after := 20, 0
	if a.has("limit") {
		if limit, err = strconv.Atoi(a.get("limit")); err != nil {
			return fmt.Errorf("--limit: %w", err)
		}
	}
	switch {
	case a.has("after"):
		if after, err = strconv.Atoi(a.get("after")); err != nil {
			return fmt.Errorf("--after: %w", err)
		}
		limit = max(limit, 1<<30)
	case a.has("new"):
		after, _ = s.bookmark(who)
	}
	events, err := s.Events(after, a.has("all"))
	if err != nil {
		return err
	}
	if len(events) > limit {
		events = events[len(events)-limit:]
	}
	if a.has("new") {
		if err := s.setBookmark(who, s.Cursor()); err != nil {
			return err
		}
	}
	if a.has("json") {
		return json.NewEncoder(os.Stdout).Encode(events)
	}
	if len(events) == 0 {
		fmt.Println("nothing new")
	}
	for _, e := range events {
		fmt.Println(e)
	}
	return nil
}

// cmdWait blocks until someone else changes the board, and cmdTail prints
// every change as it happens.
func cmdWait(a args) error { return follow(a, true) }
func cmdTail(a args) error { return follow(a, false) }

func follow(a args, once bool) error {
	if err := a.allow("as", "after", "mentions", "timeout", "all", "json"); err != nil {
		return err
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	if err := s.Sync(); err != nil {
		return err
	}
	who := author(a.get("as"))
	timeout, err := duration(a, "timeout", 9*time.Minute)
	if err != nil {
		return err
	}
	after := s.Cursor()
	switch {
	case a.has("after"):
		if after, err = strconv.Atoi(a.get("after")); err != nil {
			return fmt.Errorf("--after: %w", err)
		}
	case !once:
	case len(a.pos) > 0:
		// Waiting on a note starts from the caller's own last word there, so an
		// answer that arrived before the wait began is not missed.
		past, err := s.Events(0, true)
		if err != nil {
			return err
		}
		for _, e := range past {
			if slices.Contains(a.pos, e.ID) && strings.EqualFold(e.By, who) {
				after = e.N
			}
		}
		// And from past what an earlier wait on it already returned.
		for _, id := range a.pos {
			if seen, ok := s.bookmark(who + "@" + id); ok {
				after = max(after, seen)
			}
		}
	default:
		if seen, ok := s.bookmark(who); ok {
			after = seen
		}
		// Whatever already mentions the caller and is unanswered is what it is waiting for.
		if notes, _, err := s.Board(who); err == nil && a.has("mentions") {
			if notes = slices.DeleteFunc(notes, func(n Note) bool { return !n.NeedsMe }); len(notes) > 0 {
				if err := s.setBookmark(who, s.Cursor()); err != nil {
					return err
				}
				return cmdInbox(a.only("as", "json"))
			}
		}
	}
	deadline := time.Now().Add(timeout)
	for {
		events, err := s.Events(after, a.has("all"))
		if err != nil {
			return err
		}
		for _, e := range events {
			after = e.N
			if len(a.pos) > 0 && !slices.Contains(a.pos, e.ID) {
				continue
			}
			if a.has("mentions") && !slices.Contains(e.Mentions, strings.ToLower(who)) {
				continue
			}
			// The caller's own changes are not what it is waiting for, and neither is
			// its task coming back to it, which the answer that caused it already said.
			if once && (strings.EqualFold(e.By, who) || (strings.HasPrefix(e.Type, "task.") && strings.EqualFold(e.Holder, who))) {
				continue
			}
			if a.has("json") {
				json.NewEncoder(os.Stdout).Encode(e)
			} else {
				fmt.Println(e)
			}
			if once {
				if len(a.pos) == 0 {
					return s.setBookmark(who, e.N)
				}
				return s.setBookmark(who+"@"+e.ID, e.N)
			}
		}
		if once && time.Now().After(deadline) {
			return fmt.Errorf("timeout: nothing happened in %s. A timeout is not an answer: wait again, or do other work and check fusen inbox later", timeout)
		}
		time.Sleep(400 * time.Millisecond)
		if err := s.Sync(); err != nil {
			return err
		}
	}
}
