package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// A card can show a wedo checklist. `wedo: <list>` in a note's front matter
// names one of wedo's own lists, or a list file kept inside the project. The
// card reads and ticks it through the wedo command, so wedo stays the only
// writer of its files.

type ListItem struct {
	ID   int    `json:"id"`
	Text string `json:"text"`
	Done bool   `json:"done"`
}

// Section reports whether the item is one of wedo's headings and not a task.
func (i ListItem) Section() (string, bool) { return strings.CutPrefix(i.Text, "# ") }

// wedoList turns a card's list into the arguments that select it and the file wedo keeps it in.
func wedoList(root, ref string) (args []string, file string, err error) {
	switch {
	case ref == "" || strings.HasPrefix(ref, "-"):
		return nil, "", fmt.Errorf("%q is not a wedo list", ref)
	case strings.ContainsRune(ref, '/') || strings.HasSuffix(ref, ".json"):
		// A note is free text an agent may have written: it must not point wedo at a file elsewhere on disk.
		if !filepath.IsLocal(ref) {
			return nil, "", fmt.Errorf("wedo list file %q is outside the project", ref)
		}
		file = filepath.Join(root, ref)
		return []string{"-f", file}, file, nil
	}
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		home, _ := os.UserHomeDir()
		data = filepath.Join(home, ".local", "share")
	}
	return []string{"-l", ref}, filepath.Join(data, "wedo", ref+".json"), nil
}

func wedo(root, ref string, args ...string) ([]byte, error) {
	list, _, err := wedoList(root, ref)
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd := exec.Command("wedo", append(list, args...)...)
	cmd.Dir, cmd.Stderr = root, &stderr
	out, err := cmd.Output()
	if errors.Is(err, exec.ErrNotFound) {
		return nil, errors.New("wedo is not installed")
	}
	if err != nil {
		return nil, fmt.Errorf("wedo: %s", cmp.Or(strings.TrimSpace(stderr.String()), err.Error()))
	}
	return out, nil
}

func wedoItems(root, ref string) ([]ListItem, error) {
	out, err := wedo(root, ref, "list", "--json")
	if err != nil {
		return nil, err
	}
	var items []ListItem
	return items, json.Unmarshal(out, &items)
}

func wedoSet(root, ref string, id int, done bool) error {
	verb := "undo"
	if done {
		verb = "done"
	}
	_, err := wedo(root, ref, verb, fmt.Sprint(id))
	return err
}

// wedoFiles lists the files behind the notes' checklists, to watch for changes.
func wedoFiles(root string, notes []Note) []string {
	var files []string
	for _, n := range notes {
		if _, file, err := wedoList(root, n.Wedo); err == nil {
			files = append(files, file)
		}
	}
	slices.Sort(files)
	return slices.Compact(files)
}
