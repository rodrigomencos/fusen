package main

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/rivo/uniseg"
)

// The terminal canvas is drawn into a grid of cells, so overlapping notes and
// wide characters need no slicing of styled strings.

type attr struct {
	fg, bg                      string
	bold, italic, under, strike bool
}

type cell struct {
	s string // "" is the right half of a wide character
	a attr
}

type grid struct {
	w, h  int
	cells []cell
}

type cellRect struct{ x, y, w, h int }

func newGrid(w, h int) *grid {
	g := &grid{w: w, h: h, cells: make([]cell, w*h)}
	for i := range g.cells {
		g.cells[i].s = " "
	}
	return g
}

func (g *grid) put(x, y int, s string, width int, a attr) {
	if y < 0 || y >= g.h || x < 0 || x+width > g.w {
		return
	}
	row := g.cells[y*g.w : (y+1)*g.w]
	// Writing over half of a wide character leaves its other half blank.
	if row[x].s == "" && x > 0 {
		row[x-1].s = " "
	}
	if end := x + width; end < g.w && row[end].s == "" {
		row[end].s = " "
	}
	row[x] = cell{s, a}
	for i := 1; i < width; i++ {
		row[x+i] = cell{"", a}
	}
}

func (g *grid) fill(r cellRect, a attr) {
	for y := r.y; y < r.y+r.h; y++ {
		for x := r.x; x < r.x+r.w; x++ {
			g.put(x, y, " ", 1, a)
		}
	}
}

// text writes s from column x and returns the column after it. Text wider
// than maxW ends in an ellipsis.
func (g *grid) text(x, y, maxW int, s string, a attr) int {
	if maxW <= 0 {
		return x
	}
	if uniseg.StringWidth(s) > maxW {
		s, _ = cut(s, maxW-1)
		s += "…"
	}
	gr := uniseg.NewGraphemes(s)
	for gr.Next() {
		if w := gr.Width(); w > 0 {
			g.put(x, y, gr.Str(), w, a)
			x += w
		}
	}
	return x
}

var styles = map[attr]lipgloss.Style{}

func (g *grid) String() string {
	var b strings.Builder
	for y := 0; y < g.h; y++ {
		if y > 0 {
			b.WriteByte('\n')
		}
		row := g.cells[y*g.w : (y+1)*g.w]
		for x := 0; x < g.w; {
			a := row[x].a
			var run strings.Builder
			for ; x < g.w && row[x].a == a; x++ {
				run.WriteString(row[x].s)
			}
			if a == (attr{}) {
				b.WriteString(run.String())
				continue
			}
			st, ok := styles[a]
			if !ok {
				st = lipgloss.NewStyle().Bold(a.bold).Italic(a.italic).Underline(a.under).Strikethrough(a.strike)
				if a.fg != "" {
					st = st.Foreground(lipgloss.Color(a.fg))
				}
				if a.bg != "" {
					st = st.Background(lipgloss.Color(a.bg))
				}
				styles[a] = st
			}
			b.WriteString(st.Render(run.String()))
		}
	}
	return b.String()
}

// cut splits s after as many characters as fit in n cells.
func cut(s string, n int) (string, string) {
	w := 0
	gr := uniseg.NewGraphemes(s)
	for gr.Next() {
		if w += gr.Width(); w > n {
			from, _ := gr.Positions()
			return s[:from], s[from:]
		}
	}
	return s, ""
}

// ---------- Colors ----------

type theme struct {
	bg, ink, muted, faint, dot, accent, code, card, line string
	shadow                                               float64
}

var noteColors = []string{"", "yellow", "pink", "blue", "green", "purple", "gray"}

var stripHex = map[string]string{
	"yellow": "#e9b825", "pink": "#e2618c", "blue": "#4c8fd6",
	"green": "#4daa72", "purple": "#8c70d1", "gray": "#9a9488",
}

// detectTheme builds the palette around the terminal's own background, so
// cards sit a shade above it whatever the color scheme.
func detectTheme() theme {
	th := theme{bg: termenv.ConvertToRGB(termenv.NewOutput(os.Stdout).BackgroundColor()).Hex()}
	r, g, b := rgb(th.bg)
	if (299*r+587*g+114*b)/1000 < 128 {
		th.ink, th.accent, th.shadow = "#e9e6df", "#f08a54", .5
		th.card = mixHex(th.bg, "#ffffff", .08)
	} else {
		th.ink, th.accent, th.shadow = "#26231d", "#b4491c", .18
		th.card = mixHex(th.bg, "#ffffff", .6)
	}
	th.muted = mixHex(th.card, th.ink, .55)
	th.faint = mixHex(th.card, th.ink, .28)
	th.line = mixHex(th.card, th.ink, .16)
	th.dot = mixHex(th.bg, th.ink, .16)
	th.code = mixHex(th.ink, th.accent, .55)
	return th
}

// strip is the color of a note's top edge.
func (th theme) strip(color string) string {
	if hex, ok := stripHex[color]; ok {
		return hex
	}
	return th.line
}

func (th theme) cardFill(color string) string {
	return mixHex(th.card, th.strip(color), .09)
}

func rgb(hex string) (r, g, b int) {
	if len(hex) == 7 {
		v, _ := strconv.ParseUint(hex[1:], 16, 32)
		return int(v >> 16), int(v >> 8 & 0xff), int(v & 0xff)
	}
	return 0, 0, 0
}

// mixHex blends t of b into a.
func mixHex(a, b string, t float64) string {
	ar, ag, ab := rgb(a)
	br, bg, bb := rgb(b)
	mix := func(x, y int) int { return int(float64(x) + (float64(y)-float64(x))*t + .5) }
	return fmt.Sprintf("#%02x%02x%02x", mix(ar, br), mix(ag, bg), mix(ab, bb))
}

// ---------- Markdown ----------

var (
	reFence   = regexp.MustCompile("^\\s*```")
	reItem    = regexp.MustCompile(`^(\s*)([-*+]|\d+[.)])\s+(.*)$`)
	reTask    = regexp.MustCompile(`^\[([ xX])\]\s+(.*)$`)
	reHeading = regexp.MustCompile(`^#{1,6}\s+(.*)$`)
	reRule    = regexp.MustCompile(`^\s*(---+|\*\*\*+)\s*$`)
	reQuote   = regexp.MustCompile(`^>\s?(.*)$`)
)

type span struct {
	text string
	a    attr
}

type line struct {
	spans []span
	task  int // which task in the note's text this line opens, or -1
	item  int // which wedo item this line opens, or 0
}

// box reports whether the line starts with a checkbox.
func (l line) box() bool { return l.task >= 0 || l.item > 0 }

// wrapBlock wraps text into lines, with a prefix on the first and its width
// indented on the rest.
func wrapBlock(prefix, spans []span, width int) []line {
	pw := 0
	for _, p := range prefix {
		pw += uniseg.StringWidth(p.text)
	}
	var out []line
	for i, row := range wrap(spans, width-pw) {
		l := line{task: -1}
		if i == 0 {
			l.spans = append(l.spans, prefix...)
		} else {
			l.spans = append(l.spans, span{strings.Repeat(" ", pw), attr{}})
		}
		l.spans = append(l.spans, row...)
		out = append(out, l)
	}
	return out
}

// wedoLines lays out the wedo checklist a note shows.
func wedoLines(n Note, width int, th theme) []line {
	if n.ItemsError != "" {
		return wrapBlock(nil, []span{{n.ItemsError, attr{fg: th.accent}}}, width)
	}
	done, total := 0, 0
	var out []line
	for _, it := range n.Items {
		if title, ok := it.Section(); ok {
			if len(out) > 0 {
				out = append(out, line{task: -1})
			}
			out = append(out, wrapBlock(nil, inline(title, attr{fg: th.ink, bold: true}, th), width)...)
			continue
		}
		total++
		box, a := "[ ] ", attr{fg: th.ink}
		if it.Done {
			done++
			box, a = "[x] ", attr{fg: th.muted, strike: true}
		}
		rows := wrapBlock([]span{{"", attr{}}, {box, attr{fg: th.accent}}}, inline(it.Text, a, th), width)
		rows[0].item = it.ID
		out = append(out, rows...)
	}
	head := line{[]span{{n.Wedo, attr{fg: th.muted, bold: true}}, {fmt.Sprintf("  %d/%d", done, total), attr{fg: th.muted}}}, -1, 0}
	return append([]line{head}, out...)
}

// mdLines lays a note's Markdown out as styled lines no wider than width.
func mdLines(body string, width int, th theme) []line {
	var out []line
	tasks, fenced := 0, false
	text := attr{fg: th.ink}
	add := func(prefix, spans []span, task int) {
		rows := wrapBlock(prefix, spans, width)
		rows[0].task = task
		out = append(out, rows...)
	}
	for _, src := range strings.Split(body, "\n") {
		src = strings.ReplaceAll(src, "\t", "    ")
		var m []string
		switch {
		case reFence.MatchString(src):
			fenced = !fenced
		case fenced:
			out = append(out, line{[]span{{src, attr{fg: th.code}}}, -1, 0})
		case strings.TrimSpace(src) == "":
			if len(out) > 0 && out[len(out)-1].spans != nil {
				out = append(out, line{task: -1})
			}
		case match(reHeading, src, &m):
			add(nil, inline(m[1], attr{fg: th.ink, bold: true}, th), -1)
		case match(reItem, src, &m):
			indent := strings.Repeat(" ", len(m[1])/2*2)
			if t := reTask.FindStringSubmatch(m[3]); t != nil {
				box, a := "[ ] ", text
				if t[1] != " " {
					box, a = "[x] ", attr{fg: th.muted, strike: true}
				}
				add([]span{{indent, attr{}}, {box, attr{fg: th.accent}}}, inline(t[2], a, th), tasks)
				tasks++
			} else {
				bullet := "• "
				if m[2][0] >= '0' && m[2][0] <= '9' {
					bullet = m[2] + " "
				}
				add([]span{{indent + bullet, attr{fg: th.muted}}}, inline(m[3], text, th), -1)
			}
		case reRule.MatchString(src):
			out = append(out, line{[]span{{strings.Repeat("─", width), attr{fg: th.faint}}}, -1, 0})
		case match(reQuote, src, &m):
			add([]span{{"▎ ", attr{fg: th.faint}}}, inline(m[1], attr{fg: th.muted}, th), -1)
		default:
			add(nil, inline(src, text, th), -1)
		}
	}
	for len(out) > 0 && out[len(out)-1].spans == nil {
		out = out[:len(out)-1]
	}
	return out
}

func match(re *regexp.Regexp, s string, m *[]string) bool {
	*m = re.FindStringSubmatch(s)
	return *m != nil
}

// inline styles code, bold, italic, strikethrough and links within one line.
func inline(s string, base attr, th theme) []span {
	var out []span
	var buf strings.Builder
	cur := base
	flush := func() {
		if buf.Len() > 0 {
			out = append(out, span{buf.String(), cur})
			buf.Reset()
		}
	}
	for i := 0; i < len(s); {
		rest := s[i:]
		// A marker opens only if something follows it and it is closed later on.
		toggle := func(marker string, on *bool) bool {
			after, ok := strings.CutPrefix(rest, marker)
			if !ok || (!*on && (after == "" || after[0] == ' ' || !strings.Contains(after, marker))) {
				return false
			}
			flush()
			*on = !*on
			i += len(marker)
			return true
		}
		if toggle("**", &cur.bold) || toggle("~~", &cur.strike) || toggle("*", &cur.italic) {
			continue
		}
		if rest[0] == '`' {
			if end := strings.IndexByte(rest[1:], '`'); end > 0 {
				flush()
				out = append(out, span{rest[1 : 1+end], attr{fg: th.code}})
				i += end + 2
				continue
			}
		}
		if rest[0] == '[' {
			if mid := strings.Index(rest, "]("); mid > 1 {
				if end := strings.IndexByte(rest[mid:], ')'); end > 2 {
					flush()
					link := cur
					link.fg, link.under = th.accent, true
					out = append(out, span{rest[1:mid], link})
					i += mid + end + 1
					continue
				}
			}
		}
		_, size := utf8.DecodeRuneInString(rest)
		buf.WriteString(rest[:size])
		i += size
	}
	flush()
	return out
}

// wrap breaks styled text into rows at word boundaries, splitting only words
// that are wider than a whole row.
func wrap(spans []span, width int) [][]span {
	width = max(width, 2)
	var rows [][]span
	var row []span
	used := 0
	flush := func() {
		rows = append(rows, row)
		row, used = nil, 0
	}
	for _, sp := range spans {
		for _, word := range words(sp.text) {
			w := uniseg.StringWidth(word)
			if word[0] == ' ' {
				if used > 0 && used+w < width {
					row, used = append(row, span{word, sp.a}), used+w
				}
				continue
			}
			if used > 0 && used+w > width {
				flush()
			}
			for w > width-used {
				var head string
				head, word = cut(word, width-used)
				row = append(row, span{head, sp.a})
				flush()
				w = uniseg.StringWidth(word)
			}
			if word != "" {
				row, used = append(row, span{word, sp.a}), used+w
			}
		}
	}
	if row != nil || rows == nil {
		flush()
	}
	return rows
}

// words splits s into alternating runs of spaces and of anything else.
func words(s string) []string {
	var out []string
	start := 0
	for i := 1; i <= len(s); i++ {
		if i == len(s) || (s[i] == ' ') != (s[start] == ' ') {
			out = append(out, s[start:i])
			start = i
		}
	}
	return out
}

// toggleTask flips the nth task in a note's Markdown, counting as mdLines does.
func toggleTask(body string, n int) string {
	lines := strings.Split(body, "\n")
	k, fenced := 0, false
	for i, l := range lines {
		if reFence.MatchString(l) {
			fenced = !fenced
			continue
		}
		m := reItem.FindStringSubmatch(l)
		if fenced || m == nil || !reTask.MatchString(m[3]) {
			continue
		}
		if k++; k-1 == n {
			at := len(l) - len(m[3]) + 1
			mark := "x"
			if l[at] != ' ' {
				mark = " "
			}
			lines[i] = l[:at] + mark + l[at+1:]
			break
		}
	}
	return strings.Join(lines, "\n")
}

// ---------- Cards ----------

type cardState int

const (
	cardPlain cardState = iota
	cardSelected
	cardFlash // just changed on disk
	cardDim   // filtered out
)

// cardBody is a note's laid-out text and how many rows of it the card shows.
func cardBody(n Note, r cellRect, th theme) ([]line, int) {
	lines := mdLines(n.Body, r.w-4, th)
	if n.Wedo != "" {
		if len(lines) > 0 {
			lines = append(lines, line{task: -1})
		}
		lines = append(lines, wedoLines(n, r.w-4, th)...)
	}
	return lines, max(r.h-4, 0)
}

// drawCard paints a note's text. With images on, the card behind the text is
// a picture the terminal draws; without, it is made of colored cells here.
func drawCard(g *grid, n Note, r cellRect, st cardState, th theme, images bool) {
	bg := ""
	if !images {
		bg = th.cardFill(n.Color)
	}
	on := func(a attr) attr {
		if st == cardDim {
			a = attr{fg: th.faint}
		}
		a.bg = bg
		return a
	}
	g.fill(r, attr{bg: bg})
	if !images {
		edge := attr{fg: th.strip(n.Color), bg: bg}
		outlined := st == cardSelected || st == cardFlash
		if outlined {
			edge.fg = th.accent
		}
		for x := r.x; x < r.x+r.w; x++ {
			g.put(x, r.y, "▀", 1, edge)
			if outlined {
				g.put(x, r.y+r.h-1, "▄", 1, edge)
			}
		}
		for y := r.y; outlined && y < r.y+r.h; y++ {
			g.put(r.x, y, "▌", 1, edge)
			g.put(r.x+r.w-1, y, "▐", 1, edge)
		}
	}

	title := n.Title
	if title == "" {
		title = n.ID
	}
	// The task's state sits at the right of the title row, giving way to "needs you".
	tag, tagAttr := n.State, attr{fg: th.muted}
	switch {
	case n.NeedsMe:
		tag, tagAttr = "needs you", attr{fg: th.accent, bold: true}
	case n.State == "claimed":
		tag = "claimed " + n.Holder
	}
	titleW := r.w - 4
	if tw := uniseg.StringWidth(tag); tag != "" && tw < r.w-10 {
		g.text(r.x+r.w-2-tw, r.y+1, tw, tag, on(tagAttr))
		titleW -= tw + 1
	}
	g.text(r.x+2, r.y+1, titleW, title, on(attr{fg: th.ink, bold: true}))

	lines, rows := cardBody(n, r, th)
	if len(lines) == 0 && rows > 0 {
		g.text(r.x+2, r.y+2, r.w-4, "Empty note", on(attr{fg: th.muted, italic: true}))
	}
	hidden := 0
	if len(lines) > rows {
		hidden = len(lines) - max(rows-1, 0)
		lines = lines[:max(rows-1, 0)]
	}
	for i, l := range lines {
		x := r.x + 2
		for _, sp := range l.spans {
			x = g.text(x, r.y+2+i, r.x+r.w-2-x, sp.text, on(sp.a))
		}
	}
	if hidden > 0 && rows > 0 {
		g.text(r.x+2, r.y+2+len(lines), r.w-4, fmt.Sprintf("… %d more lines", hidden), on(attr{fg: th.muted}))
	}

	foot := r.y + r.h - 2
	meta := ago(n.Updated)
	if n.By != "" {
		meta = n.By + " · " + meta
	}
	end := g.text(r.x+2, foot, r.w-6, meta, on(attr{fg: th.muted}))
	if len(n.Labels) > 0 {
		tags := "#" + strings.Join(n.Labels, " #")
		if x := r.x + r.w - 4 - uniseg.StringWidth(tags); x > end+1 {
			g.text(x, foot, r.w, tags, on(attr{fg: mixHex(th.muted, th.strip(n.Color), .5)}))
		}
	}
	g.put(r.x+r.w-2, foot, "◢", 1, on(attr{fg: th.faint}))
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < 45*time.Second:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()+.5))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()+.5))
	case d < 14*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24+.5))
	}
	return t.Local().Format("Jan 2, 2006")
}
