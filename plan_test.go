package main

import (
	"slices"
	"testing"
	"time"
)

func mustPlan(t *testing.T, s Store, src string) []Note {
	t.Helper()
	plan, err := parsePlan([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.addPlan(plan, "claude")
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

func TestPlan(t *testing.T) {
	s := testBoard(t)
	save(t, s, Note{ID: "audit", Title: "Audit the old table", Task: "ready"})

	saved := mustPlan(t, s, `[
		{"key": "schema", "title": "Write the migration", "for": "codex", "labels": ["db"]},
		{"key": "backfill", "title": "Backfill old rows", "needs": ["schema"]},
		{"title": "Switch reads", "body": "Behind the flag.", "needs": ["schema", "backfill", "audit"]}
	]`)

	ids := []string{}
	for _, n := range saved {
		ids = append(ids, n.ID)
	}
	if want := []string{"write-the-migration", "backfill-old-rows", "switch-reads"}; !slices.Equal(ids, want) {
		t.Fatalf("ids %v, want %v", ids, want)
	}
	if got := saved[2].Needs; !slices.Equal(got, []string{"write-the-migration", "backfill-old-rows", "audit"}) {
		t.Fatalf("keys were not turned into ids: %v", got)
	}
	if saved[0].For != "codex" || !slices.Equal(saved[0].Labels, []string{"db"}) || saved[2].Body != "Behind the flag." || saved[1].By != "claude" {
		t.Fatalf("fields were not kept: %+v", saved)
	}
	for id, want := range map[string]string{"write-the-migration": "ready", "backfill-old-rows": "waiting", "switch-reads": "waiting"} {
		if got := stateOf(t, s, id); got != want {
			t.Errorf("%s is %q, want %q", id, got, want)
		}
	}
	layout, _ := s.Layout()
	for _, id := range ids {
		if _, ok := layout[id]; !ok {
			t.Errorf("%s has no place on the canvas", id)
		}
	}
}

func TestPlanOrder(t *testing.T) {
	s := testBoard(t)
	// Listed against the alphabet, so only the plan's order can explain which is taken first.
	mustPlan(t, s, `[{"title": "Zip the logs"}, {"title": "Archive the zips"}]`)
	n, err := s.claimNext("worker", time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n.ID != "zip-the-logs" {
		t.Fatalf("claimed %s first, want zip-the-logs", n.ID)
	}
}

func TestPlanRejected(t *testing.T) {
	cases := map[string]string{
		"unknown need":  `[{"title": "A", "needs": ["nowhere"]}]`,
		"plain note":    `[{"title": "A", "needs": ["idea"]}]`,
		"canceled need": `[{"title": "A", "needs": ["dropped"]}]`,
		"duplicate key": `[{"key": "a", "title": "A"}, {"key": "a", "title": "B"}]`,
		"no title":      `[{"key": "a", "title": "A"}, {"title": " "}]`,
		"loop":          `[{"key": "a", "title": "A", "needs": ["c"]}, {"key": "b", "title": "B", "needs": ["a"]}, {"key": "c", "title": "C", "needs": ["b"]}]`,
		"needs itself":  `[{"key": "a", "title": "A", "needs": ["a"]}]`,
		"bad field":     `[{"title": "A", "need": ["idea"]}]`,
		"empty":         `[]`,
		"not a list":    `{"title": "A"}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			s := testBoard(t)
			save(t, s, Note{ID: "idea", Title: "An idea"}, Note{ID: "dropped", Title: "Dropped", Task: "canceled"})
			before, _ := s.List()
			plan, err := parsePlan([]byte(src))
			if err == nil {
				_, err = s.addPlan(plan, "claude")
			}
			wantErr(t, err, "bad_plan")
			if after, _ := s.List(); len(after) != len(before) {
				t.Fatalf("a rejected plan left %d notes behind", len(after)-len(before))
			}
		})
	}
}
