package main

import (
	"slices"
	"testing"
	"time"
)

func syncBoard(t *testing.T, s Store) {
	t.Helper()
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
}

// eventsAfter returns the log after cursor n, renewals included.
func eventsAfter(t *testing.T, s Store, n int) []Event {
	t.Helper()
	events, err := s.Events(n, true)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestExpiredClaimIsLogged(t *testing.T) {
	s := testBoard(t)
	syncBoard(t, s) // takes stock of the empty board
	ranOut := time.Now().Add(-time.Minute).Truncate(time.Second)
	save(t, s, Note{ID: "fix", Title: "Fix it", Task: "claimed", Holder: "codex", By: "codex", Until: ranOut})
	syncBoard(t, s)

	var expired []Event
	for _, e := range eventsAfter(t, s, 0) {
		if e.Type == "task.expired" {
			expired = append(expired, e)
		}
	}
	if len(expired) != 1 {
		t.Fatalf("got %d task.expired events, want 1: %v", len(expired), eventsAfter(t, s, 0))
	}
	if e := expired[0]; e.ID != "fix" || e.Holder != "codex" || e.By != "" || !e.At.Equal(ranOut) {
		t.Fatalf("expired event reads %+v", e)
	}

	// Only once: looking again finds nothing new.
	cursor := s.Cursor()
	syncBoard(t, s)
	if events := eventsAfter(t, s, cursor); len(events) != 0 {
		t.Fatalf("a second look logged %v", events)
	}

	// Taken again by the same agent, that is a new claim, and it can run out again.
	change(t, s, "fix", func(n *Note, notes []Note) error { return claim(n, notes, "codex", time.Minute) })
	syncBoard(t, s)
	change(t, s, "fix", func(n *Note, _ []Note) error { n.Until = time.Now().Add(-time.Second); return nil })
	syncBoard(t, s)
	var types []string
	for _, e := range eventsAfter(t, s, cursor) {
		types = append(types, e.Type)
	}
	if want := []string{"task.claimed", "task.renewed", "task.expired"}; !slices.Equal(types, want) {
		t.Fatalf("got %v, want %v", types, want)
	}
}

func TestLiveClaimIsNotExpired(t *testing.T) {
	s := testBoard(t)
	syncBoard(t, s)
	save(t, s, Note{ID: "fix", Title: "Fix it", Task: "ready"})
	change(t, s, "fix", func(n *Note, notes []Note) error { return claim(n, notes, "codex", time.Minute) })
	syncBoard(t, s)
	change(t, s, "fix", func(n *Note, _ []Note) error { return renew(n, "codex", time.Minute) })
	syncBoard(t, s)
	for _, e := range eventsAfter(t, s, 0) {
		if e.Type == "task.expired" {
			t.Fatalf("a live claim was logged as expired: %v", e)
		}
	}
}

// What was there before the log's first look is not news, even a claim that already ran out.
func TestFirstLookTakesStock(t *testing.T) {
	s := testBoard(t)
	save(t, s, Note{ID: "fix", Title: "Fix it", Task: "claimed", Holder: "codex", Until: time.Now().Add(-time.Hour)})
	syncBoard(t, s)
	syncBoard(t, s)
	if events := eventsAfter(t, s, 0); len(events) != 0 {
		t.Fatalf("got %v, want nothing", events)
	}
}
