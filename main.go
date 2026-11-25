package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"
)

const usage = `fusen: a board of sticky notes that lives in your project, kept by you and your agents

Usage:
  fusen                      open this project's board in the browser
  fusen tui [--graphics on|off]
                             open it in the terminal instead
  fusen serve [--port N] [--no-open]
  fusen init                 create .fusen/ in the current directory
  fusen ls [--label L] [--sort newest|oldest|changed|title] [--json]
  fusen read <id>... [--json]
  fusen add <title> [body|-] [--as NAME] [--label L]... [--color C] [--wedo LIST]
  fusen edit <id>            open the note in $EDITOR
  fusen edit <id> [body|-] [--append] [--title T] [--as NAME] [--label L]... [--color C] [--wedo LIST]
  fusen rm <id>...
  fusen reply <id> <text|->  add a signed reply under a note

Tasks (a note with a task state is work someone can claim):
  fusen tasks [--all|--done] open tasks and who holds them; --done lists
                             finished ones, latest first
  fusen add <title> [body] --task [--needs ID]... [--for NAME]
  fusen plan <file|-> [--json]
                             add several tasks at once from a JSON list; a
                             task's needs can name others in it by key
  fusen claim [<id>] [--lease 15m] [--wait 10m] [--label L]...
                             take a task, or the oldest ready one
  fusen renew <id>           keep a claim past its lease
  fusen done <id> [result]   finish it
  fusen fail <id> <reason>
  fusen cancel <id> <reason> call it off: nobody should do it any more
  fusen release <id>         give it back untouched
  fusen block <id> <question> [--kind needs_input|awaiting_approval] [--ask NAME]
                             park it on a question for a person
  fusen unblock <id>         (a reply from someone else also unblocks)
  fusen reopen <id>          back in the queue; also makes a plain note a task
  fusen run [<id>] -- <command>...
                             claim, run the command, settle by its exit code

Following changes:
  fusen inbox                notes that mention you and are unanswered
  fusen log [--new] [--after N] [--limit N]
                             what changed; --new is since you last looked
  fusen wait [<id>...] [--mentions] [--timeout 9m]
                             return when someone else changes something
  fusen tail [<id>...]       print changes as they happen

Agents:
  fusen skill [show|setup|remove|status]
                             the guide that teaches agents to keep a board

--wedo shows a wedo checklist on the card: one of wedo's lists by name, or a
list file inside the project. A body of "-" is read from standard input. --as says who is writing (default
$FUSEN_AS, then $USER).

Notes are Markdown files in .fusen/notes/, found from the current directory
upward. Agents can also read and edit those files directly: see .fusen/README.md.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "fusen:", err)
		os.Exit(1)
	}
}

func run(argv []string) error {
	cmd := "serve"
	if len(argv) > 0 && !strings.HasPrefix(argv[0], "--") {
		cmd, argv = argv[0], argv[1:]
	}
	a, err := parseArgs(argv)
	if err != nil {
		return err
	}
	if a.has("help") || cmd == "help" {
		fmt.Print(usage)
		return nil
	}
	err = dispatch(cmd, a)
	// Whatever just changed, by this command or by hand since the last one, goes into the log.
	if cwd, cwdErr := os.Getwd(); cwdErr == nil && cmd != "skill" {
		if s, ok := findBoard(cwd); ok {
			err = cmp.Or(err, s.Sync())
		}
	}
	return err
}

func dispatch(cmd string, a args) error {
	switch cmd {
	case "serve":
		return cmdServe(a)
	case "tui":
		return cmdTUI(a)
	case "init":
		return cmdInit(a)
	case "ls":
		return cmdList(a)
	case "read":
		return cmdRead(a)
	case "add":
		return cmdAdd(a)
	case "edit":
		return cmdEdit(a)
	case "rm":
		return cmdRemove(a)
	case "tasks":
		return cmdTasks(a)
	case "claim":
		return cmdClaim(a)
	case "plan":
		return cmdPlan(a)
	case "renew", "release", "done", "fail", "cancel", "block", "unblock", "reopen", "reply":
		return cmdTask(cmd, a)
	case "run":
		return cmdRun(a)
	case "inbox":
		return cmdInbox(a)
	case "log":
		return cmdLog(a)
	case "wait":
		return cmdWait(a)
	case "tail":
		return cmdTail(a)
	case "skill":
		return cmdSkill(a)
	}
	return fmt.Errorf("unknown command %q (try fusen help)", cmd)
}

type args struct {
	pos   []string
	flags map[string][]string
	rest  []string // everything after a bare --, for commands that run another command
}

var boolFlags = []string{"json", "append", "no-open", "help", "task", "all", "done", "new", "mentions"}

// parseArgs accepts flags anywhere among the positional arguments.
func parseArgs(in []string) (args, error) {
	a := args{flags: map[string][]string{}}
	for i := 0; i < len(in); i++ {
		s := in[i]
		if s == "--" {
			a.rest = in[i+1:]
			break
		}
		if !strings.HasPrefix(s, "--") {
			a.pos = append(a.pos, s)
			continue
		}
		name, val, hasVal := strings.Cut(s[2:], "=")
		if slices.Contains(boolFlags, name) {
			val = "true"
		} else if !hasVal {
			if i+1 >= len(in) {
				return a, fmt.Errorf("--%s needs a value", name)
			}
			i++
			val = in[i]
		}
		a.flags[name] = append(a.flags[name], val)
	}
	return a, nil
}

func (a args) has(name string) bool { return len(a.flags[name]) > 0 }

func (a args) get(name string) string {
	if v := a.flags[name]; len(v) > 0 {
		return v[len(v)-1]
	}
	return ""
}

// only keeps the named flags, to hand the arguments on to another command.
func (a args) only(names ...string) args {
	kept := args{flags: map[string][]string{}}
	for _, name := range names {
		if v, ok := a.flags[name]; ok {
			kept.flags[name] = v
		}
	}
	return kept
}

// allow rejects flags the command does not take, so a typo is not ignored.
func (a args) allow(names ...string) error {
	for name := range a.flags {
		if !slices.Contains(names, name) {
			return fmt.Errorf("unknown flag --%s", name)
		}
	}
	return nil
}

// author names whoever is writing: the name given, else $FUSEN_AS, else $USER.
func author(as string) string {
	return cmp.Or(as, os.Getenv("FUSEN_AS"), os.Getenv("USER"))
}

// editorCmd opens a file in the person's own editor.
func editorCmd(path string) *exec.Cmd {
	parts := strings.Fields(cmp.Or(os.Getenv("VISUAL"), os.Getenv("EDITOR"), "vi"))
	cmd := exec.Command(parts[0], append(parts[1:], path)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd
}

func cmdTUI(a args) error {
	if err := a.allow("graphics"); err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	store, ok := findBoard(cwd)
	if !ok {
		if store, err = initBoard(cwd); err != nil {
			return err
		}
	}
	return runTUI(store, cmp.Or(a.get("graphics"), os.Getenv("FUSEN_GRAPHICS")))
}

func openStore() (Store, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return Store{}, err
	}
	if s, ok := findBoard(cwd); ok {
		return s, nil
	}
	return Store{}, errors.New("no .fusen here or in any parent directory (run fusen init)")
}

func readBody(arg string) (string, error) {
	if arg != "-" {
		return arg, nil
	}
	data, err := io.ReadAll(os.Stdin)
	return string(data), err
}

func cmdInit(a args) error {
	if err := a.allow(); err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	s, err := initBoard(cwd)
	if err != nil {
		return err
	}
	fmt.Printf("Board ready in %s\n\nTo point agents at it, add this to the project's CLAUDE.md or AGENTS.md:\n\n"+
		"  This project keeps shared notes on a board in .fusen/. Read .fusen/README.md before using it.\n", s.dir)
	return nil
}

func cmdServe(a args) error {
	if err := a.allow("port", "no-open"); err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	store, ok := findBoard(cwd)
	if !ok {
		if store, err = initBoard(cwd); err != nil {
			return err
		}
		fmt.Printf("Created %s\n", store.dir)
	}
	root := store.root()

	// Each project gets its own steady port, so its address and saved view stay put.
	h := fnv.New32a()
	h.Write([]byte(root))
	port := 4700 + int(h.Sum32()%300)
	if a.has("port") {
		if port, err = strconv.Atoi(a.get("port")); err != nil {
			return fmt.Errorf("--port: %w", err)
		}
	}
	show := func(url string) {
		if !a.has("no-open") {
			openBrowser(url)
		}
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		url := fmt.Sprintf("http://127.0.0.1:%d", port)
		if servesBoard(url, root) {
			fmt.Printf("Already running at %s\n", url)
			show(url)
			return nil
		}
		if a.has("port") {
			return err
		}
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return err
		}
	}
	url := "http://" + ln.Addr().String()
	fmt.Printf("Board for %s\n%s\n", root, url)
	show(url)
	return serve(store, ln)
}

// servesBoard reports whether a board server for root already answers at url.
func servesBoard(url, root string) bool {
	client := http.Client{Timeout: time.Second}
	resp, err := client.Get(url + "/api/board")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var state boardState
	return json.NewDecoder(resp.Body).Decode(&state) == nil && state.Root == root
}

func openBrowser(url string) {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	exec.Command(name, url).Start()
}

func cmdList(a args) error {
	if err := a.allow("label", "sort", "json"); err != nil {
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
	if a.has("label") {
		notes = slices.DeleteFunc(notes, func(n Note) bool { return !slices.Contains(n.Labels, a.get("label")) })
	}
	if a.has("sort") {
		if err := sortNotes(notes, a.get("sort")); err != nil {
			return err
		}
	}
	if a.has("json") {
		return json.NewEncoder(os.Stdout).Encode(notes)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTITLE\tBY\tUPDATED\tTASK\tLABELS")
	for _, n := range notes {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", n.ID, n.Title, n.By, n.Updated.Local().Format("2006-01-02 15:04"), stateText(n), strings.Join(n.Labels, ", "))
	}
	return w.Flush()
}

func cmdRead(a args) error {
	if err := a.allow("json"); err != nil {
		return err
	}
	if len(a.pos) == 0 {
		return errors.New("usage: fusen read <id>...")
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	notes := []Note{}
	for _, id := range a.pos {
		n, err := s.Get(id)
		if err != nil {
			return err
		}
		notes = append(notes, n)
	}
	if a.has("json") {
		return json.NewEncoder(os.Stdout).Encode(notes)
	}
	for i, n := range notes {
		if len(notes) > 1 {
			if i > 0 {
				fmt.Println()
			}
			fmt.Printf("== %s ==\n", n.ID)
		}
		os.Stdout.Write(n.encode())
	}
	return nil
}

func cmdAdd(a args) error {
	if err := a.allow("as", "label", "color", "wedo", "task", "needs", "for"); err != nil {
		return err
	}
	if len(a.pos) < 1 || len(a.pos) > 2 {
		return errors.New("usage: fusen add <title> [body|-]")
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	n := Note{Title: a.pos[0], By: author(a.get("as")), Labels: a.flags["label"], Color: a.get("color"), Wedo: a.get("wedo"),
		Needs: a.flags["needs"], For: a.get("for"), Created: time.Now()}
	if a.has("task") || n.Needs != nil || n.For != "" {
		n.Task = "ready"
	}
	if len(a.pos) == 2 {
		if n.Body, err = readBody(a.pos[1]); err != nil {
			return err
		}
	}
	if n.ID, err = s.NewID(n.Title); err != nil {
		return err
	}
	if _, err := s.Save(n); err != nil {
		return err
	}
	fmt.Println(n.ID)
	return nil
}

func cmdEdit(a args) error {
	if err := a.allow("as", "label", "color", "wedo", "title", "append", "needs", "for"); err != nil {
		return err
	}
	if len(a.pos) < 1 || len(a.pos) > 2 {
		return errors.New("usage: fusen edit <id> [body|-]")
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	if len(a.pos) == 1 && !a.has("title") && !a.has("label") && !a.has("color") && !a.has("wedo") && !a.has("needs") && !a.has("for") {
		// Only a person at a terminal gets an editor: an agent would hang in it.
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return errors.New("nothing to change: give a body, --title, --label, --color or --wedo")
		}
		if _, err := s.Get(a.pos[0]); err != nil {
			return err
		}
		return editorCmd(s.path(a.pos[0])).Run()
	}
	body := ""
	if len(a.pos) == 2 {
		if body, err = readBody(a.pos[1]); err != nil {
			return err
		}
	}
	_, err = s.change(a.pos[0], func(n *Note, _ []Note) error {
		if len(a.pos) == 2 {
			if a.has("append") && n.Body != "" {
				body = n.Body + "\n" + body
			}
			n.Body = body
		}
		if a.has("title") {
			n.Title = a.get("title")
		}
		if a.has("label") {
			n.Labels = a.flags["label"]
		}
		if a.has("color") {
			n.Color = a.get("color")
		}
		if a.has("wedo") {
			n.Wedo = a.get("wedo")
		}
		if a.has("needs") {
			// --needs "" clears them.
			n.Needs = slices.DeleteFunc(slices.Clone(a.flags["needs"]), func(id string) bool { return id == "" })
		}
		if a.has("for") {
			n.For = a.get("for")
		}
		n.By = author(a.get("as"))
		return nil
	})
	return err
}

func cmdRemove(a args) error {
	if err := a.allow(); err != nil {
		return err
	}
	if len(a.pos) == 0 {
		return errors.New("usage: fusen rm <id>...")
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	for _, id := range a.pos {
		if err := s.Delete(id); err != nil {
			return err
		}
	}
	return nil
}
