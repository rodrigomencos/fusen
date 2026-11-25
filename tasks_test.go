package main

import (
	"strings"
	"testing"
	"time"
)

// testBoard makes an empty board in a temporary directory.
func testBoard(t *testing.T) Store {
	t.Helper()
	s, err := initBoard(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func save(t *testing.T, s Store, notes ...Note) {
	t.Helper()
	for _, n := range notes {
		if _, err := s.Save(n); err != nil {
			t.Fatal(err)
		}
	}
}

func change(t *testing.T, s Store, id string, fn func(n *Note, notes []Note) error) Note {
	t.Helper()
	n, err := s.change(id, fn)
	if err != nil {
		t.Fatalf("change %s: %v", id, err)
	}
	return n
}

// stateOf is a task's state as everyone sees it right now.
func stateOf(t *testing.T, s Store, id string) string {
	t.Helper()
	notes, _, err := s.Board("")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range notes {
		if n.ID == id {
			return n.State
		}
	}
	t.Fatalf("no note %s", id)
	return ""
}

func wantErr(t *testing.T, err error, prefix string) {
	t.Helper()
	if err == nil || !strings.HasPrefix(err.Error(), prefix) {
		t.Fatalf("got error %v, want one starting with %s", err, prefix)
	}
}

func TestCancel(t *testing.T) {
	s := testBoard(t)
	save(t, s,
		Note{ID: "schema", Title: "Write the migration", Task: "ready"},
		Note{ID: "deploy", Title: "Deploy it", Task: "ready", Needs: []string{"schema"}},
	)
	change(t, s, "schema", func(n *Note, notes []Note) error { return claim(n, notes, "codex", time.Minute) })

	// The person calls it off while codex holds it.
	n := change(t, s, "schema", func(n *Note, _ []Note) error { return cancel(n, "alex") })
	if got := stateOf(t, s, "schema"); got != "canceled" {
		t.Fatalf("state %q, want canceled", got)
	}
	if !n.finished() || n.Holder != "" || n.Finished.IsZero() || n.By != "alex" {
		t.Fatalf("canceled task reads %+v", n)
	}

	// Codex hears it the next time it renews or settles.
	_, err := s.change("schema", func(n *Note, _ []Note) error { return renew(n, "codex", time.Minute) })
	wantErr(t, err, "task_canceled")
	_, err = s.change("schema", func(n *Note, notes []Note) error { return held(n, notes, "codex") })
	wantErr(t, err, "task_canceled")

	// Nothing that needs it can start, and the board can say which tasks those are.
	if got := stateOf(t, s, "deploy"); got != "waiting" {
		t.Fatalf("dependent state %q, want waiting", got)
	}
	notes, _ := s.List()
	if deps := dependents("schema", notes); len(deps) != 1 || deps[0].ID != "deploy" {
		t.Fatalf("dependents %v, want [deploy]", deps)
	}

	// It cannot be canceled twice, and reopening puts it back in the queue.
	_, err = s.change("schema", func(n *Note, _ []Note) error { return cancel(n, "alex") })
	wantErr(t, err, "task_canceled")
	n = change(t, s, "schema", func(n *Note, _ []Note) error { settle(n, "alex", "ready"); return nil })
	if got := stateOf(t, s, "schema"); got != "ready" || !n.Finished.IsZero() {
		t.Fatalf("reopened task is %q with finished %v", got, n.Finished)
	}
}

func TestCancelPlainNote(t *testing.T) {
	s := testBoard(t)
	save(t, s, Note{ID: "idea", Title: "An idea"})
	_, err := s.change("idea", func(n *Note, _ []Note) error { return cancel(n, "alex") })
	wantErr(t, err, "not_a_task")
}
