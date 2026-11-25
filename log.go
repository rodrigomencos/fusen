package main

import (
	"bufio"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The board keeps a log of what changed, in order, so an agent can ask what
// is new since it last looked and can wait for something to happen.
//
// Notes are plain files that anyone may edit directly, so the log is not
// written by the commands that change them. Sync compares the notes with what
// they were the last time it ran and records the differences, whoever made
// them and however.

// Event is one change to a note.
type Event struct {
	N        int       `json:"n"` // the event's place in the log, and the cursor to read on from
	At       time.Time `json:"at"`
	Type     string    `json:"type"` // created, edited, deleted, replied, task.<state>, task.renewed or task.expired
	ID       string    `json:"id"`
	Title    string    `json:"title,omitempty"`
	By       string    `json:"by,omitempty"`
	Holder   string    `json:"holder,omitempty"`
	Kind     string    `json:"kind,omitempty"`     // why a task is blocked
	Mentions []string  `json:"mentions,omitempty"` // names this change newly mentions
	Text     string    `json:"text,omitempty"`     // a reply's text
}

func (e Event) String() string {
	s := fmt.Sprintf("#%d %s %s %s", e.N, e.At.Local().Format("15:04"), e.Type, e.ID)
	if e.Type == "task.expired" {
		return s + " (" + e.Holder + "'s claim ran out)"
	}
	if e.Kind != "" {
		s += " (" + e.Kind + ")"
	}
	if who := cmp.Or(e.Holder, e.By); who != "" {
		s += " by " + who
	}
	for _, m := range e.Mentions {
		if e.Text == "" {
			s += " @" + m
		}
	}
	if e.Text != "" {
		text, more := cut(strings.Join(strings.Fields(e.Text), " "), 200)
		if more != "" {
			text += "…"
		}
		s += ": " + text
	}
	return s
}

// snapshot is what Sync remembers of a note, to tell what changed.
type snapshot struct {
	Rev      string         `json:"rev"`
	Title    string         `json:"title"`
	Text     string         `json:"text"` // hash of what a reader sees: title and body
	Task     string         `json:"task,omitempty"`
	Holder   string         `json:"holder,omitempty"`
	Until    time.Time      `json:"until,omitzero"`    // when a claim runs out
	Expired  bool           `json:"expired,omitempty"` // that it ran out was logged
	Replies  int            `json:"replies,omitempty"`
	Mentions map[string]int `json:"mentions,omitempty"`
}

type logState struct {
	Next  int                 `json:"next"`
	Notes map[string]snapshot `json:"notes"`
}

func (s Store) local(name string) string { return s.dir + "/.local/" + name }

func take(n Note) snapshot {
	sum := sha256.Sum256([]byte(n.Title + "\n" + n.Body))
	snap := snapshot{Rev: n.Rev, Title: cmp.Or(n.Title, n.ID), Text: hex.EncodeToString(sum[:6]), Task: n.Task, Holder: n.Holder, Until: n.Until}
	snap.Replies = len(reReply.FindAllString(n.Body, -1))
	for _, name := range mentions(n.Body) {
		if snap.Mentions == nil {
			snap.Mentions = map[string]int{}
		}
		snap.Mentions[name]++
	}
	return snap
}

// changes names what happened to a note whose file changed since old was taken.
func changes(old, cur snapshot, known bool) []string {
	var types []string
	if !known {
		types = append(types, "created")
	}
	if known && cur.Replies > old.Replies {
		types = append(types, "replied")
	}
	// A claim taken after the last one ran out is a new claim, even by the same holder.
	reclaimed := old.Expired && cur.Task == "claimed" && !cur.Until.Equal(old.Until)
	if cur.Task != "" && (cur.Task != old.Task || cur.Holder != old.Holder || reclaimed) {
		types = append(types, "task."+cur.Task)
	}
	if len(types) == 0 {
		types = []string{"edited"}
		// A longer lease is the only thing that changes a held task without changing its text.
		if cur.Task == "claimed" && cur.Text == old.Text {
			types = []string{"task.renewed"}
		}
	}
	return types
}

// Sync brings the log up to date with the notes on disk.
func (s Store) Sync() error {
	return s.locked(func() error {
		var st logState
		data, err := os.ReadFile(s.local("state.json"))
		first := err != nil
		if err == nil {
			if err := json.Unmarshal(data, &st); err != nil {
				return fmt.Errorf("%s: %w", s.local("state.json"), err)
			}
		}
		notes, err := s.List()
		if err != nil {
			return err
		}
		var events []Event
		now := map[string]snapshot{}
		for _, n := range notes {
			cur := take(n)
			old, known := st.Notes[n.ID]
			var types []string
			if known && old.Rev == cur.Rev {
				cur.Expired = old.Expired
			} else {
				types = changes(old, cur, known)
			}
			// A claim that runs out changes no file, so only the clock tells, once.
			if cur.Task == "claimed" && !cur.Until.IsZero() && time.Now().After(cur.Until) && !cur.Expired {
				cur.Expired = true
				types = append(types, "task.expired")
			}
			now[n.ID] = cur
			for i, t := range types {
				e := Event{Type: t, ID: n.ID, Title: cur.Title, By: n.By}
				if t == "task.expired" {
					e.By, e.At = "", cur.Until // nobody did it, and it happened then, not when noticed
				}
				if i == 0 {
					for name, count := range cur.Mentions {
						if count > old.Mentions[name] {
							e.Mentions = append(e.Mentions, name)
						}
					}
					slices.Sort(e.Mentions)
				}
				if t == "replied" {
					e.Text = lastReply(n.Body)
				}
				if strings.HasPrefix(t, "task.") {
					e.Holder, e.Kind = n.Holder, n.Blocked
				}
				events = append(events, e)
			}
		}
		for id, old := range st.Notes {
			if _, ok := now[id]; !ok {
				events = append(events, Event{Type: "deleted", ID: id, Title: old.Title})
			}
		}
		// The first run only takes stock: everything already there is not news.
		if first {
			events = nil
		} else if len(events) == 0 {
			return nil
		}
		slices.SortStableFunc(events, func(a, b Event) int { return cmp.Compare(a.ID, b.ID) })
		var lines strings.Builder
		for _, e := range events {
			st.Next++
			e.N, e.At = st.Next, cmp.Or(e.At, time.Now())
			line, _ := json.Marshal(e)
			lines.Write(append(line, '\n'))
		}
		if lines.Len() > 0 {
			f, err := os.OpenFile(s.local("log.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			_, err = f.WriteString(lines.String())
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		}
		st.Notes = now
		data, _ = json.Marshal(st)
		return writeAtomic(s.local("state.json"), data)
	})
}

// Events returns what the log holds after the given cursor. Lease renewals
// are bookkeeping and are left out unless all is set.
func (s Store) Events(after int, all bool) ([]Event, error) {
	f, err := os.Open(s.local("log.jsonl"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var events []Event
	lines := bufio.NewScanner(f)
	lines.Buffer(nil, 1<<20)
	for lines.Scan() {
		var e Event
		if json.Unmarshal(lines.Bytes(), &e) == nil && e.N > after && (all || e.Type != "task.renewed") {
			events = append(events, e)
		}
	}
	return events, lines.Err()
}

// Cursor is the place of the newest event in the log.
func (s Store) Cursor() int {
	var st logState
	if data, err := os.ReadFile(s.local("state.json")); err == nil {
		json.Unmarshal(data, &st)
	}
	return st.Next
}

// Each name has a bookmark: the last event its owner was shown. It is what
// lets an agent ask for only what is new without keeping a cursor itself.

func (s Store) bookmark(who string) (int, bool) {
	data, err := os.ReadFile(s.local("seen/" + strings.ToLower(who)))
	n, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
	return n, err == nil && convErr == nil
}

func (s Store) setBookmark(who string, n int) error {
	if !validID(who) {
		return fmt.Errorf("invalid name %q", who)
	}
	if err := os.MkdirAll(s.local("seen"), 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.local("seen/"+strings.ToLower(who)), []byte(strconv.Itoa(n)), 0o644)
}
