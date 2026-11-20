package main

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/rivo/uniseg"
	"golang.org/x/term"
)

// Canvas positions are stored in the web page's pixels, so both views share
// one layout. A terminal cell stands for this many of them.
const (
	cellPxW, cellPxH   = 8.0, 18.0
	minCardW, minCardH = 20, 6
)

type mode int

const (
	modeCanvas mode = iota
	modeEdit
	modeFilter
	modeConfirm // deleting the selected note
	modeReply   // typing a reply to the selected note
)

type point struct{ x, y int }

type dragKind int

const (
	dragPan dragKind = iota
	dragMove
	dragSize
)

type drag struct {
	kind  dragKind
	id    string
	from  point // pointer cell at press
	cam   point // camera at press
	rect  Rect  // the note's place at press
	box   *line // checkbox under the pointer at press, if any
	moved bool
}

type editor struct {
	id      string // "" for a note that has no file yet
	rev     string // what the note was when opened
	at      point  // canvas cell where a new note goes
	title   textinput.Model
	body    textarea.Model
	labels  textinput.Model
	color   string
	wedo    textinput.Model
	focus   int  // 0 title, 1 body, 2 labels, 3 wedo list, 4 color
	changed bool // the file changed on disk since it was opened
	warned  bool // the person was told so; the next save replaces it
}

type tickMsg time.Time
type editorDoneMsg struct{ err error }

type tui struct {
	store Store
	th    theme
	gfx   *kitty // nil when the terminal draws no pictures
	away  bool   // the terminal is with an editor, or closing: draw no pictures

	notes  map[string]Note
	layout map[string]Rect
	order  []string // back to front
	sig    string
	flash  map[string]time.Time // notes that just changed on disk, until when to show it

	w, h   int
	cam    point // canvas cell at the top-left of the screen
	aimed  bool  // the camera has been pointed at the notes
	sel    string
	mode   mode
	filter string
	typed  string // the reply being typed
	drag   *drag
	click  struct {
		at time.Time
		id string
		p  point
	}
	ed          *editor
	status      string
	statusUntil time.Time
}

func runTUI(store Store, graphics string) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return errors.New("fusen tui needs a terminal")
	}
	tty := &lockedTTY{File: os.Stdout}
	m := &tui{store: store, th: detectTheme(), flash: map[string]time.Time{}}
	if graphicsSupported(graphics) {
		m.gfx = newKitty(tty, m.th)
	}
	if err := m.reload(false); err != nil {
		return err
	}
	_, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithOutput(tty)).Run()
	return err
}

func tick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *tui) Init() tea.Cmd { return tick() }

// reload reads the board from disk. With flash set, notes that changed since
// the last read are highlighted for a moment.
func (m *tui) reload(flash bool) error {
	m.report(m.store.Sync())
	sig := m.signature()
	notes, layout, err := m.store.Board(author(""))
	if err != nil {
		return err
	}
	next := map[string]Note{}
	for _, n := range notes {
		next[n.ID] = n
		if old, ok := m.notes[n.ID]; flash && (!ok || old.Rev != n.Rev || !slices.Equal(old.Items, n.Items)) {
			m.flash[n.ID] = time.Now().Add(1500 * time.Millisecond)
		}
		if !slices.Contains(m.order, n.ID) {
			m.order = append(m.order, n.ID)
		}
		if m.ed != nil && m.ed.id == n.ID && n.Rev != m.ed.rev {
			m.ed.changed = true
		}
	}
	m.order = slices.DeleteFunc(m.order, func(id string) bool { _, ok := next[id]; return !ok })
	if m.drag != nil && m.drag.kind != dragPan {
		if r, ok := m.layout[m.drag.id]; ok {
			layout[m.drag.id] = r
		}
	}
	m.notes, m.layout, m.sig = next, layout, sig
	if _, ok := next[m.sel]; !ok {
		m.sel = ""
	}
	return nil
}

// signature covers the notes, the layout and the wedo lists the notes show.
func (m *tui) signature() string {
	notes := make([]Note, 0, len(m.notes))
	for _, n := range m.notes {
		notes = append(notes, n)
	}
	return m.store.Signature(wedoFiles(m.store.root(), notes))
}

// report shows an error, or a message, in the status bar for a while.
func (m *tui) report(err error) {
	if err != nil {
		m.say(err.Error())
	}
}

func (m *tui) say(s string) {
	m.status, m.statusUntil = s, time.Now().Add(6*time.Second)
}

// ---------- Geometry ----------

func (m *tui) drawerWidth() int {
	if m.w < 70 {
		return m.w
	}
	return min(max(m.w*2/5, 44), 76)
}

func (m *tui) canvasSize() (int, int) {
	w := m.w
	if m.mode == modeEdit {
		w -= m.drawerWidth()
	}
	return max(w, 0), max(m.h-1, 0)
}

// rect is where a note sits on screen, in cells.
func (m *tui) rect(id string) cellRect {
	r := m.layout[id]
	cells := func(px, per float64) int { return int(math.Round(px / per)) }
	return cellRect{
		cells(r.X, cellPxW) - m.cam.x, cells(r.Y, cellPxH) - m.cam.y,
		max(cells(r.W, cellPxW), minCardW), max(cells(r.H, cellPxH), minCardH),
	}
}

func (m *tui) state(id string) cardState {
	switch {
	case id == m.sel:
		return cardSelected
	case time.Now().Before(m.flash[id]):
		return cardFlash
	case !m.matches(m.notes[id]):
		return cardDim
	}
	return cardPlain
}

func (m *tui) matches(n Note) bool {
	parts := append([]string{n.ID, n.Title, n.By, n.Body, n.Wedo}, n.Labels...)
	for _, it := range n.Items {
		parts = append(parts, it.Text)
	}
	text := strings.ToLower(strings.Join(parts, "\n"))
	return strings.Contains(text, strings.ToLower(m.filter))
}

// hit finds the topmost note under a screen cell.
func (m *tui) hit(p point) (string, cellRect) {
	for i := len(m.order) - 1; i >= 0; i-- {
		r := m.rect(m.order[i])
		if p.x >= r.x && p.x < r.x+r.w && p.y >= r.y && p.y < r.y+r.h {
			return m.order[i], r
		}
	}
	return "", cellRect{}
}

// boxAt finds the line whose checkbox is under a screen cell.
func (m *tui) boxAt(id string, r cellRect, p point) *line {
	lines, rows := cardBody(m.notes[id], r, m.th)
	i := p.y - r.y - 2
	if len(lines) > rows {
		rows-- // the last row says how many lines are hidden
	}
	if i < 0 || i >= len(lines) || i >= rows || !lines[i].box() {
		return nil
	}
	if x := p.x - r.x - 2 - uniseg.StringWidth(lines[i].spans[0].text); x < 0 || x > 2 {
		return nil
	}
	return &lines[i]
}

// fit points the camera at the top-left of the notes.
func (m *tui) fit() {
	m.cam = point{}
	left, top := math.MaxInt, math.MaxInt
	for id := range m.notes {
		r := m.rect(id)
		left, top = min(left, r.x), min(top, r.y)
	}
	if len(m.notes) > 0 {
		m.cam = point{left - 2, top - 1}
	}
}

// reveal moves the camera only as far as it takes to bring a note fully into view.
func (m *tui) reveal(id string) {
	r := m.rect(id)
	w, h := m.canvasSize()
	if r.x < 1 || r.x+r.w > w-1 {
		m.cam.x += r.x - max((w-r.w)/2, 1)
	}
	if r.y < 1 || r.y+r.h > h-1 {
		m.cam.y += r.y - max((h-r.h)/2, 1)
	}
}

func (m *tui) raise(id string) {
	m.order = append(slices.DeleteFunc(m.order, func(o string) bool { return o == id }), id)
}

// ---------- Update ----------

func (m *tui) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		if m.gfx != nil {
			m.gfx.reset()
		}
		if !m.aimed {
			m.fit()
			m.aimed = true
		}
		m.sizeEditor()
	case tickMsg:
		if m.signature() != m.sig {
			m.report(m.reload(true))
		}
		for id, until := range m.flash {
			if time.Now().After(until) {
				delete(m.flash, id)
			}
		}
		cmd = tick()
	case editorDoneMsg:
		m.away = false
		m.report(msg.err)
		m.report(m.reload(false))
	case tea.KeyMsg:
		cmd = m.key(msg)
	case tea.MouseMsg:
		cmd = m.mouse(msg)
	default:
		if m.ed != nil {
			cmd = m.ed.update(msg)
		}
	}
	m.syncImages()
	return m, cmd
}

func (m *tui) syncImages() {
	if m.gfx == nil || m.away || m.w == 0 {
		return
	}
	cards := make([]placement, 0, len(m.order))
	for i, id := range m.order {
		r := m.rect(id)
		cards = append(cards, placement{id, cardKey{r.w, r.h, m.notes[id].Color, m.state(id)}, r.x, r.y, i - len(m.order)})
	}
	w, h := m.canvasSize()
	m.gfx.sync(cards, w, h)
}

func (m *tui) quit() tea.Cmd {
	m.leave()
	return tea.Quit
}

// leave takes the pictures down before the terminal goes to something else.
func (m *tui) leave() {
	m.away = true
	if m.gfx != nil {
		m.gfx.reset()
	}
}

func (m *tui) key(msg tea.KeyMsg) tea.Cmd {
	k := msg.String()
	m.statusUntil = time.Time{} // a message stays until the next key
	switch m.mode {
	case modeEdit:
		return m.keyEdit(msg)
	case modeConfirm:
		m.mode = modeCanvas
		if k == "y" {
			m.report(m.store.Delete(m.sel))
			m.report(m.reload(false))
		}
		return nil
	case modeFilter, modeReply:
		text := &m.filter
		if m.mode == modeReply {
			text = &m.typed
		}
		switch {
		case k == "ctrl+c":
			return m.quit()
		case k == "enter":
			if m.mode == modeReply && strings.TrimSpace(m.typed) != "" {
				_, err := m.store.change(m.sel, func(n *Note, _ []Note) error {
					answer(n, author(""), m.typed)
					return nil
				})
				m.report(err)
				m.report(m.reload(false))
			}
			m.mode, m.typed = modeCanvas, ""
		case k == "esc":
			*text, m.mode = "", modeCanvas
		case k == "backspace":
			_, size := utf8.DecodeLastRuneInString(*text)
			*text = (*text)[:len(*text)-size]
		case msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace:
			*text += string(msg.Runes)
		}
		return nil
	}

	switch k {
	case "q", "ctrl+c":
		return m.quit()
	case "left", "h":
		m.cam.x -= 4
	case "right", "l":
		m.cam.x += 4
	case "up", "k":
		m.cam.y -= 2
	case "down", "j":
		m.cam.y += 2
	case "shift+left", "H":
		m.adjust(-2, 0, 0, 0)
	case "shift+right", "L":
		m.adjust(2, 0, 0, 0)
	case "shift+up", "K":
		m.adjust(0, -1, 0, 0)
	case "shift+down", "J":
		m.adjust(0, 1, 0, 0)
	case "ctrl+left":
		m.adjust(0, 0, -2, 0)
	case "ctrl+right":
		m.adjust(0, 0, 2, 0)
	case "ctrl+up":
		m.adjust(0, 0, 0, -1)
	case "ctrl+down":
		m.adjust(0, 0, 0, 1)
	case "tab":
		m.cycle(1)
	case "shift+tab":
		m.cycle(-1)
	case "f":
		m.fit()
	case "/":
		m.mode = modeFilter
	case "esc":
		if m.filter != "" {
			m.filter = ""
		} else {
			m.sel = ""
		}
	case "i":
		m.nextWaiting()
	case "n":
		w, h := m.canvasSize()
		return m.openEditor(Note{}, point{m.cam.x + w/2 - 19, m.cam.y + h/2 - 6})
	}
	n, ok := m.notes[m.sel]
	if !ok {
		return nil
	}
	switch k {
	case "enter", "e":
		return m.openEditor(n, point{})
	case "E":
		m.leave()
		return tea.ExecProcess(editorCmd(m.store.path(n.ID)), func(err error) tea.Msg { return editorDoneMsg{err} })
	case "d", "backspace", "delete":
		m.mode = modeConfirm
	case "r":
		m.mode = modeReply
	case "t":
		// Not a task, then ready, then done, and round again.
		next := map[string]string{"": "ready", "done": ""}[n.Task]
		if n.Task != "" && n.Task != "done" {
			next = "done"
		}
		_, err := m.store.change(n.ID, func(n *Note, _ []Note) error {
			settle(n, author(""), next)
			return nil
		})
		m.report(err)
		m.report(m.reload(false))
	case "c":
		n.Color = noteColors[(slices.Index(noteColors, n.Color)+1)%len(noteColors)]
		m.write(n)
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		var boxes []line
		lines, _ := cardBody(n, m.rect(n.ID), m.th)
		for _, l := range lines {
			if l.box() {
				boxes = append(boxes, l)
			}
		}
		if i := int(k[0] - '1'); i < len(boxes) {
			m.tick(n, boxes[i])
		}
	}
	return nil
}

// nextWaiting selects the next note that is waiting on the person.
func (m *tui) nextWaiting() {
	var ids []string
	for id, n := range m.notes {
		if n.NeedsMe {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		m.say("Nothing is waiting on you.")
		return
	}
	slices.Sort(ids)
	m.sel = ids[(slices.Index(ids, m.sel)+1)%len(ids)]
	m.raise(m.sel)
	m.reveal(m.sel)
}

// cycle selects the next note in reading order.
func (m *tui) cycle(step int) {
	if len(m.order) == 0 {
		return
	}
	ids := slices.Clone(m.order)
	sort.Slice(ids, func(i, j int) bool {
		a, b := m.layout[ids[i]], m.layout[ids[j]]
		if a.Y != b.Y {
			return a.Y < b.Y
		}
		return a.X < b.X
	})
	i := slices.Index(ids, m.sel)
	if i < 0 && step < 0 {
		i = 0
	}
	m.sel = ids[(i+step+len(ids))%len(ids)]
	m.raise(m.sel)
	m.reveal(m.sel)
}

// adjust moves or resizes the selected note by whole cells.
func (m *tui) adjust(dx, dy, dw, dh int) {
	r, ok := m.layout[m.sel]
	if !ok {
		return
	}
	r.X += float64(dx) * cellPxW
	r.Y += float64(dy) * cellPxH
	r.W = max(r.W+float64(dw)*cellPxW, minCardW*cellPxW)
	r.H = max(r.H+float64(dh)*cellPxH, minCardH*cellPxH)
	m.layout[m.sel] = r
	m.savePlace(m.sel)
}

func (m *tui) savePlace(id string) {
	layout, err := m.store.Layout()
	if err == nil {
		layout[id] = m.layout[id]
		err = m.store.SaveLayout(layout)
	}
	m.report(err)
}

// write saves a note as this person and rereads the board, so the change does
// not come back as someone else's.
func (m *tui) write(n Note) {
	n.By = author("")
	_, err := m.store.Save(n)
	m.report(err)
	m.report(m.reload(false))
}

// tick flips the checkbox a line starts with: a task in the note's own text,
// or an item of the wedo list it shows.
func (m *tui) tick(n Note, l line) {
	if i := slices.IndexFunc(n.Items, func(it ListItem) bool { return it.ID == l.item }); l.item > 0 && i >= 0 {
		m.report(wedoSet(m.store.root(), n.Wedo, l.item, !n.Items[i].Done))
		m.report(m.reload(false))
	} else if body := toggleTask(n.Body, l.task); l.item == 0 && body != n.Body {
		n.Body = body
		m.write(n)
	}
}

func (m *tui) mouse(e tea.MouseMsg) tea.Cmd {
	p := point{e.X, e.Y}
	switch e.Button {
	case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
		step := 2
		if e.Button == tea.MouseButtonWheelUp {
			step = -2
		}
		if e.Shift {
			m.cam.x += step * 2
		} else {
			m.cam.y += step
		}
		return nil
	case tea.MouseButtonWheelLeft:
		m.cam.x -= 4
		return nil
	case tea.MouseButtonWheelRight:
		m.cam.x += 4
		return nil
	}

	switch {
	case e.Action == tea.MouseActionPress && e.Button == tea.MouseButtonLeft:
		w, h := m.canvasSize()
		if m.mode == modeEdit || m.mode == modeConfirm || m.mode == modeReply || p.x >= w || p.y >= h {
			return nil
		}
		m.mode = modeCanvas
		id, r := m.hit(p)
		again := time.Since(m.click.at) < 400*time.Millisecond && m.click.id == id && m.click.p == p
		m.click.at, m.click.id, m.click.p = time.Now(), id, p
		if id == "" {
			if again {
				return m.openEditor(Note{}, point{m.cam.x + p.x - 2, m.cam.y + p.y - 1})
			}
			m.drag = &drag{kind: dragPan, from: p, cam: m.cam}
			return nil
		}
		m.sel = id
		m.raise(id)
		if again {
			return m.openEditor(m.notes[id], point{})
		}
		d := &drag{kind: dragMove, id: id, from: p, rect: m.layout[id], box: m.boxAt(id, r, p)}
		if p.x >= r.x+r.w-3 && p.y >= r.y+r.h-2 {
			d.kind = dragSize
		}
		m.drag = d
	case e.Action == tea.MouseActionMotion && m.drag != nil:
		d := m.drag
		dx, dy := p.x-d.from.x, p.y-d.from.y
		if dx == 0 && dy == 0 && !d.moved {
			return nil
		}
		d.moved = true
		r := d.rect
		switch d.kind {
		case dragPan:
			m.cam = point{d.cam.x - dx, d.cam.y - dy}
			return nil
		case dragMove:
			r.X += float64(dx) * cellPxW
			r.Y += float64(dy) * cellPxH
		case dragSize:
			r.W = max(r.W+float64(dx)*cellPxW, minCardW*cellPxW)
			r.H = max(r.H+float64(dy)*cellPxH, minCardH*cellPxH)
		}
		m.layout[d.id] = r
	case e.Action == tea.MouseActionRelease && m.drag != nil:
		d := m.drag
		m.drag = nil
		switch {
		case d.kind == dragPan && !d.moved:
			m.sel = ""
		case d.kind != dragPan && d.moved:
			m.savePlace(d.id)
		case d.box != nil:
			m.tick(m.notes[d.id], *d.box)
		}
	}
	return nil
}

// ---------- Editor ----------

func (m *tui) openEditor(n Note, at point) tea.Cmd {
	ed := &editor{id: n.ID, rev: n.Rev, at: at, color: n.Color}
	ed.title = textinput.New()
	ed.title.Prompt, ed.title.Placeholder = "", "Title"
	ed.title.SetValue(n.Title)
	ed.labels = textinput.New()
	ed.labels.Prompt, ed.labels.Placeholder = "", "ops, staging"
	ed.labels.SetValue(strings.Join(n.Labels, ", "))
	ed.wedo = textinput.New()
	ed.wedo.Prompt, ed.wedo.Placeholder = "", "none"
	ed.wedo.SetValue(n.Wedo)
	ed.body = textarea.New()
	ed.body.Prompt, ed.body.Placeholder = "", "Markdown"
	ed.body.ShowLineNumbers = false
	ed.body.CharLimit, ed.body.MaxHeight = 0, 0
	ed.body.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ed.body.SetValue(n.Body)
	m.ed, m.mode = ed, modeEdit
	m.sizeEditor()
	if n.ID == "" {
		return ed.setFocus(0)
	}
	m.reveal(n.ID)
	return ed.setFocus(1)
}

// The drawer's rows other than the note text: headings, single-line fields, gaps and the hint.
const drawerChrome = 17

func (m *tui) sizeEditor() {
	if m.ed == nil {
		return
	}
	inner := m.drawerWidth() - 3
	m.ed.title.Width, m.ed.labels.Width, m.ed.wedo.Width = inner-1, inner-1, inner-1
	m.ed.body.SetWidth(inner)
	m.ed.body.SetHeight(max(m.h-1-drawerChrome, 3))
}

func (ed *editor) setFocus(i int) tea.Cmd {
	ed.focus = i % 5
	ed.title.Blur()
	ed.body.Blur()
	ed.labels.Blur()
	ed.wedo.Blur()
	switch ed.focus {
	case 0:
		return ed.title.Focus()
	case 1:
		return ed.body.Focus()
	case 2:
		return ed.labels.Focus()
	case 3:
		return ed.wedo.Focus()
	}
	return nil
}

func (ed *editor) update(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	switch ed.focus {
	case 0:
		ed.title, cmd = ed.title.Update(msg)
	case 1:
		ed.body, cmd = ed.body.Update(msg)
	case 2:
		ed.labels, cmd = ed.labels.Update(msg)
	case 3:
		ed.wedo, cmd = ed.wedo.Update(msg)
	}
	return cmd
}

func (m *tui) keyEdit(msg tea.KeyMsg) tea.Cmd {
	ed := m.ed
	switch k := msg.String(); {
	case k == "ctrl+c":
		return m.quit()
	case k == "esc":
		m.ed, m.mode = nil, modeCanvas
		return nil
	case k == "ctrl+s":
		m.saveEditor()
		return nil
	case k == "tab", k == "enter" && ed.focus != 1:
		return ed.setFocus(ed.focus + 1)
	case k == "shift+tab":
		return ed.setFocus(ed.focus + 4)
	case ed.focus == 4:
		step := map[string]int{"left": len(noteColors) - 1, "h": len(noteColors) - 1, "right": 1, "l": 1, " ": 1}[k]
		ed.color = noteColors[(slices.Index(noteColors, ed.color)+step)%len(noteColors)]
		return nil
	}
	return ed.update(msg)
}

func (m *tui) saveEditor() {
	ed := m.ed
	n := Note{}
	if ed.id != "" {
		cur, err := m.store.Get(ed.id)
		switch {
		case errors.Is(err, os.ErrNotExist):
			n.ID = ed.id // deleted meanwhile: saving brings it back
		case err != nil:
			m.report(err)
			return
		case cur.Rev != ed.rev && !ed.warned:
			// Someone wrote the note after it was opened here. Say so once before replacing their text.
			ed.changed, ed.warned = true, true
			m.say("This note changed on disk. Save again to replace it, or esc to keep what is on disk.")
			return
		default:
			n = cur
		}
	}
	n.Title = strings.TrimSpace(ed.title.Value())
	n.Body = ed.body.Value()
	n.Labels = parseLabels(ed.labels.Value())
	n.Color = ed.color
	n.Wedo = strings.TrimSpace(ed.wedo.Value())
	if n.ID == "" {
		id, err := m.store.NewID(n.Title)
		if err != nil {
			m.report(err)
			return
		}
		n.ID = id
		m.layout[id] = Rect{float64(ed.at.x) * cellPxW, float64(ed.at.y) * cellPxH, 304, 216}
		m.savePlace(id)
	}
	m.ed, m.mode, m.sel = nil, modeCanvas, n.ID
	m.write(n)
	m.raise(n.ID)
}

// ---------- View ----------

func (m *tui) View() string {
	if m.w == 0 || m.h < 2 {
		return ""
	}
	w, h := m.canvasSize()
	g := newGrid(w, h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if mod(x+m.cam.x, 6) == 0 && mod(y+m.cam.y, 3) == 0 {
				g.put(x, y, "·", 1, attr{fg: m.th.dot})
			}
		}
	}
	for _, id := range m.order {
		drawCard(g, m.notes[id], m.rect(id), m.state(id), m.th, m.gfx != nil)
	}
	if len(m.notes) == 0 {
		hint := "No notes yet. Press n to write one."
		g.text((w-len(hint))/2, h/2, w, hint, attr{fg: m.th.muted})
	}
	view := g.String()
	if m.mode == modeEdit {
		if w == 0 {
			view = m.drawer(m.drawerWidth(), h)
		} else {
			view = lipgloss.JoinHorizontal(lipgloss.Top, view, m.drawer(m.drawerWidth(), h))
		}
	}
	return view + "\n" + m.statusBar()
}

func mod(a, b int) int { return (a%b + b) % b }

func (m *tui) drawer(w, h int) string {
	ed, color := m.ed, func(hex string) lipgloss.Style { return lipgloss.NewStyle().Foreground(lipgloss.Color(hex)) }
	label := func(i int, s string) string {
		if ed.focus == i {
			return color(m.th.accent).Bold(true).Render(s)
		}
		return color(m.th.muted).Render(s)
	}
	var swatches strings.Builder
	for _, c := range noteColors {
		dot := color(m.th.strip(c)).Render("●")
		if c == "" {
			dot = color(m.th.muted).Render("○")
		}
		if c == ed.color {
			swatches.WriteString("[" + dot + "]")
		} else {
			swatches.WriteString(" " + dot + " ")
		}
	}
	head := "Edit note"
	if ed.id == "" {
		head = "New note"
	}
	hint := color(m.th.muted).Render("ctrl+s save · esc cancel · tab next field")
	if ed.changed {
		hint = color(m.th.accent).Render("Changed on disk since you opened it")
	}
	rows := []string{
		lipgloss.NewStyle().Bold(true).Render(head), "",
		label(0, "Title"), ed.title.View(), "",
		label(1, "Note, in Markdown"), ed.body.View(), "",
		label(2, "Labels, separated by commas"), ed.labels.View(), "",
		label(3, "wedo list to show on the card"), ed.wedo.View(), "",
		label(4, "Color, ←/→ to change"), swatches.String(), "",
		hint,
	}
	return lipgloss.NewStyle().Width(w-1).Height(h).MaxHeight(h).MaxWidth(w).Padding(0, 1).
		Border(lipgloss.NormalBorder(), false, false, false, true).BorderForeground(lipgloss.Color(m.th.line)).
		Render(strings.Join(rows, "\n"))
}

func (m *tui) statusBar() string {
	left := fmt.Sprintf(" %s  %d notes", filepath.Base(m.store.root()), len(m.notes))
	if len(m.notes) == 1 {
		left = strings.TrimSuffix(left, "s")
	}
	if m.filter != "" && m.mode != modeFilter {
		left += "  filter: " + m.filter
	}
	waiting := 0
	for _, n := range m.notes {
		if n.NeedsMe {
			waiting++
		}
	}
	if waiting > 0 {
		left += fmt.Sprintf("  %d waiting on you (i)", waiting)
	}
	right, loud := "n new  tab select  arrows pan  / filter  f fit  q quit", false
	switch {
	case m.mode == modeConfirm:
		n := m.notes[m.sel]
		right, loud = fmt.Sprintf("Delete %q? y deletes the file, any other key keeps it", cmp.Or(n.Title, n.ID)), true
	case m.mode == modeFilter:
		right, loud = "/"+m.filter+"█  enter keeps the filter, esc clears it", true
	case m.mode == modeReply:
		right, loud = "reply: "+m.typed+"█  enter sends, esc cancels", true
	case time.Now().Before(m.statusUntil):
		right, loud = m.status, true
	case m.mode == modeEdit:
		right = "ctrl+s save  esc cancel  tab next field"
	case m.sel != "":
		right = "e edit  r reply  t task  c color  d delete  1-9 tick  E $EDITOR  shift+arrows move  ctrl+arrows resize"
	}
	right, _ = cut(right, max(m.w-uniseg.StringWidth(left)-3, 0))
	gap := strings.Repeat(" ", max(m.w-uniseg.StringWidth(left)-uniseg.StringWidth(right)-1, 1))
	style := lipgloss.NewStyle().Foreground(lipgloss.Color(m.th.muted))
	if loud {
		return style.Render(left+gap) + lipgloss.NewStyle().Foreground(lipgloss.Color(m.th.accent)).Render(right)
	}
	return style.Render(left + gap + right)
}
