package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
)

// A plan lays out several tasks at once, as a JSON list. A task can need
// others in the same plan by key, before any of them has an id, or notes
// already on the board by id:
//
//	[
//	  {"key": "schema", "title": "Write the migration", "for": "codex"},
//	  {"title": "Backfill old rows", "needs": ["schema"]}
//	]
//
// The whole plan is checked before anything is written, and written under the
// board's lock, so nobody can claim part of a plan that then fails.

type planTask struct {
	Key    string   `json:"key"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels"`
	Needs  []string `json:"needs"`
	For    string   `json:"for"`
	Color  string   `json:"color"`
}

// planned is a task the plan added, with the key the plan gave it.
type planned struct {
	Key string `json:"key,omitempty"`
	Note
}

func parsePlan(data []byte) ([]planTask, error) {
	var plan []planTask
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields() // so a misspelt "need" is not silently ignored
	if err := dec.Decode(&plan); err != nil {
		return nil, fmt.Errorf("bad_plan: a plan is a JSON list of tasks: %w", err)
	}
	if len(plan) == 0 {
		return nil, errors.New("bad_plan: the plan has no tasks")
	}
	return plan, nil
}

// addPlan checks a plan and writes its tasks, in order, as one change.
func (s Store) addPlan(plan []planTask, who string) ([]Note, error) {
	var saved []Note
	err := s.locked(func() error {
		notes, err := s.List()
		if err != nil {
			return err
		}
		keys := map[string]int{}
		for i, t := range plan {
			if strings.TrimSpace(t.Title) == "" {
				return fmt.Errorf("bad_plan: task %d has no title", i+1)
			}
			if t.Key == "" {
				continue
			}
			if _, dup := keys[t.Key]; dup {
				return fmt.Errorf("bad_plan: two tasks have the key %q", t.Key)
			}
			keys[t.Key] = i
		}
		for _, t := range plan {
			for _, need := range t.Needs {
				if _, ok := keys[need]; ok {
					continue // a key in the plan comes before a note of the same id
				}
				j := slices.IndexFunc(notes, func(n Note) bool { return n.ID == need })
				switch {
				case j < 0:
					return fmt.Errorf("bad_plan: %q needs %q, which is neither a key in this plan nor a note on the board", t.Title, need)
				case notes[j].Task == "":
					return fmt.Errorf("bad_plan: %q needs %q, which is a plain note and so is never done", t.Title, need)
				case notes[j].Task == "canceled":
					return fmt.Errorf("bad_plan: %q needs %q, which was canceled", t.Title, need)
				}
			}
		}
		if loop := planLoop(plan, keys); loop != nil {
			return fmt.Errorf("bad_plan: these tasks need each other, so none of them could start: %s", strings.Join(loop, " → "))
		}

		layout, err := s.Layout()
		if err != nil {
			return err
		}
		ids := make([]string, len(plan))
		// A plan that fails halfway takes back the files it already made.
		defer func() {
			if len(saved) < len(plan) {
				for _, id := range ids {
					if id != "" {
						os.Remove(s.path(id))
					}
				}
				saved = nil
			}
		}()
		for i, t := range plan {
			if ids[i], err = s.NewID(t.Title); err != nil {
				return err
			}
		}
		start := time.Now()
		for i, t := range plan {
			n := Note{ID: ids[i], Title: t.Title, Body: t.Body, Labels: t.Labels, Color: t.Color,
				For: t.For, By: who, Task: "ready",
				// A second apart, ending now, so claim takes them in the plan's order.
				Created: start.Add(time.Duration(i-len(plan)+1) * time.Second)}
			for _, need := range t.Needs {
				if j, ok := keys[need]; ok {
					need = ids[j]
				}
				n.Needs = append(n.Needs, need)
			}
			// Placed in the plan's order, so the canvas reads like the plan.
			layout[n.ID] = freeSlot(layout)
			if n, err = s.Save(n); err != nil {
				return err
			}
			saved = append(saved, n)
		}
		return s.SaveLayout(layout)
	})
	return saved, err
}

// planLoop finds tasks in a plan that need each other in a loop, which none
// of them could ever leave, and names them by key.
func planLoop(plan []planTask, keys map[string]int) []string {
	const (
		unseen = iota
		visiting
		visited
	)
	mark := make([]int, len(plan))
	var path []int
	var visit func(i int) []string
	visit = func(i int) []string {
		mark[i] = visiting
		path = append(path, i)
		for _, need := range plan[i].Needs {
			j, ok := keys[need]
			if !ok {
				continue
			}
			switch mark[j] {
			case visiting:
				var loop []string
				for _, k := range path[slices.Index(path, j):] {
					loop = append(loop, plan[k].Key)
				}
				return append(loop, plan[j].Key)
			case unseen:
				if loop := visit(j); loop != nil {
					return loop
				}
			}
		}
		mark[i] = visited
		path = path[:len(path)-1]
		return nil
	}
	for i := range plan {
		if mark[i] == unseen {
			if loop := visit(i); loop != nil {
				return loop
			}
		}
	}
	return nil
}

func cmdPlan(a args) error {
	if err := a.allow("as", "json"); err != nil {
		return err
	}
	if len(a.pos) != 1 {
		return errors.New("usage: fusen plan <file|->")
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	var data []byte
	if a.pos[0] == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(a.pos[0])
	}
	if err != nil {
		return err
	}
	plan, err := parsePlan(data)
	if err != nil {
		return err
	}
	who := author(a.get("as"))
	saved, err := s.addPlan(plan, who)
	if err != nil {
		return err
	}
	notes, _, err := s.Board(who)
	if err != nil {
		return err
	}
	out := make([]planned, len(saved))
	for i, n := range saved {
		if j := slices.IndexFunc(notes, func(o Note) bool { return o.ID == n.ID }); j >= 0 {
			n = notes[j]
		}
		out[i] = planned{Key: plan[i].Key, Note: n}
	}
	if a.has("json") {
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "KEY\tID\tSTATE\tTITLE")
	for _, p := range out {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.Key, p.ID, stateText(p.Note), p.Title)
	}
	return w.Flush()
}
