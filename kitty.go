package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// Cards are drawn with the kitty graphics protocol, which Ghostty and kitty
// both speak: each card is a picture placed underneath the terminal's text.

// lockedTTY lets pictures and Bubble Tea's frames share the terminal without
// one landing in the middle of the other.
type lockedTTY struct {
	*os.File
	mu sync.Mutex
}

func (t *lockedTTY) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.File.Write(p)
}

func (t *lockedTTY) WriteString(s string) (int, error) { return t.Write([]byte(s)) }

// graphicsSupported asks the terminal whether it draws kitty graphics. A
// terminal that does answers the query; every terminal answers the
// device-attributes request sent after it, which is what ends the wait.
func graphicsSupported(pref string) bool {
	switch pref {
	case "on":
		return true
	case "off":
		return false
	}
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return false
	}
	defer term.Restore(fd, old)
	os.Stdout.WriteString("\x1b_Gi=31,s=1,v=1,a=q,t=d,f=24;AAAA\x1b\\\x1b[c")
	reply := make(chan string, 1)
	go func() {
		var got []byte
		buf := make([]byte, 64)
		for {
			n, err := os.Stdin.Read(buf)
			got = append(got, buf[:n]...)
			at := bytes.Index(got, []byte("\x1b[?"))
			if err != nil || (at >= 0 && bytes.IndexByte(got[at:], 'c') > 0) {
				reply <- string(got)
				return
			}
		}
	}()
	select {
	case r := <-reply:
		return strings.Contains(r, "_Gi=31;OK")
	case <-time.After(time.Second):
		return false
	}
}

// cardKey names one card picture: every note of this size, color and state shares it.
type cardKey struct {
	w, h  int // cells
	color string
	state cardState
}

type placement struct {
	id   string // note
	key  cardKey
	x, y int // cell of the card's top-left corner, which may be off screen
	z    int
}

type shown struct {
	image, placement int
	cmd              string
}

type kitty struct {
	out           io.Writer
	th            theme
	cellW, cellH  int // pixels
	images        map[cardKey]int
	shown         map[string]shown
	lastImage     int
	lastPlacement int
}

func newKitty(out io.Writer, th theme) *kitty {
	k := &kitty{out: out, th: th}
	k.reset()
	return k
}

// reset removes every picture and forgets them, for when the terminal may
// have dropped them or changed its cell size.
func (k *kitty) reset() {
	io.WriteString(k.out, "\x1b_Ga=d,d=A,q=2\x1b\\")
	k.images, k.shown = map[cardKey]int{}, map[string]shown{}
	// Placements are scaled to whole cells, so a wrong guess here only costs sharpness.
	k.cellW, k.cellH = 10, 22
	ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err == nil && ws.Col > 0 && ws.Row > 0 && ws.Xpixel >= ws.Col && ws.Ypixel >= ws.Row {
		k.cellW, k.cellH = int(ws.Xpixel/ws.Col), int(ws.Ypixel/ws.Row)
	}
}

// sync makes the terminal show exactly these cards, clipped to the canvas,
// sending only what changed since the last call.
func (k *kitty) sync(cards []placement, canvasW, canvasH int) {
	var b strings.Builder
	used := map[int]bool{}
	visible := map[string]bool{}
	for _, c := range cards {
		x0, y0 := max(c.x, 0), max(c.y, 0)
		x1, y1 := min(c.x+c.key.w, canvasW), min(c.y+c.key.h, canvasH)
		if x0 >= x1 || y0 >= y1 {
			continue
		}
		visible[c.id] = true
		img, ok := k.images[c.key]
		if !ok {
			k.lastImage++
			img = k.lastImage
			k.images[c.key] = img
			transmit(&b, img, cardPNG(c.key, k.cellW, k.cellH, k.th))
		}
		used[img] = true
		prev, had := k.shown[c.id]
		if !had {
			k.lastPlacement++
			prev.placement = k.lastPlacement
		}
		// The source rectangle crops the picture to the part of the card on screen.
		cmd := fmt.Sprintf("\x1b[%d;%dH\x1b_Ga=p,i=%d,p=%d,x=%d,y=%d,w=%d,h=%d,c=%d,r=%d,z=%d,C=1,q=2\x1b\\",
			y0+1, x0+1, img, prev.placement,
			(x0-c.x)*k.cellW, (y0-c.y)*k.cellH, (x1-x0)*k.cellW, (y1-y0)*k.cellH, x1-x0, y1-y0, c.z)
		if had && prev.cmd == cmd {
			continue
		}
		if had && prev.image != img {
			fmt.Fprintf(&b, "\x1b_Ga=d,d=i,i=%d,p=%d,q=2\x1b\\", prev.image, prev.placement)
		}
		b.WriteString(cmd)
		k.shown[c.id] = shown{img, prev.placement, cmd}
	}
	for id, s := range k.shown {
		if !visible[id] {
			fmt.Fprintf(&b, "\x1b_Ga=d,d=i,i=%d,p=%d,q=2\x1b\\", s.image, s.placement)
			delete(k.shown, id)
		}
	}
	for key, img := range k.images {
		if !used[img] {
			fmt.Fprintf(&b, "\x1b_Ga=d,d=I,i=%d,q=2\x1b\\", img)
			delete(k.images, key)
		}
	}
	if b.Len() > 0 {
		io.WriteString(k.out, "\x1b7"+b.String()+"\x1b8")
	}
}

// transmit sends a PNG to the terminal under an id, in the chunks the protocol asks for.
func transmit(b *strings.Builder, id int, data []byte) {
	enc := base64.StdEncoding.EncodeToString(data)
	for first := true; len(enc) > 0; first = false {
		chunk := enc[:min(len(enc), 4096)]
		enc = enc[len(chunk):]
		more := 0
		if len(enc) > 0 {
			more = 1
		}
		if first {
			fmt.Fprintf(b, "\x1b_Ga=t,f=100,i=%d,q=2,m=%d;%s\x1b\\", id, more, chunk)
		} else {
			fmt.Fprintf(b, "\x1b_Gm=%d;%s\x1b\\", more, chunk)
		}
	}
}

// cardPNG draws a card: a rounded sheet with a colored top edge, a border and
// a soft shadow, on a transparent background.
func cardPNG(key cardKey, cellW, cellH int, th theme) []byte {
	w, h := key.w*cellW, key.h*cellH
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	s := float64(cellH) / 18 // 1 on a standard display, about 2 on a high-density one

	// The sheet sits inside the card's cells, leaving room below for the shadow.
	left, right := .3*float64(cellW), float64(w)-.3*float64(cellW)
	top, bottom := .25*float64(cellH), float64(h)-.5*float64(cellH)
	radius, strip, border := 7*s, 3.5*s, s
	fill := th.cardFill(key.color)
	edge := mixHex(th.line, th.strip(key.color), .35)
	stripColor := th.strip(key.color)
	switch key.state {
	case cardSelected:
		edge, stripColor, border = th.accent, th.accent, 2*s
	case cardFlash:
		edge, stripColor, border = th.accent, th.accent, 3*s
	}
	opacity := 1.0
	if key.state == cardDim {
		opacity = .3
	}

	// Signed distance from a point to the sheet's rounded outline: negative inside.
	dist := func(x, y float64) float64 {
		qx := math.Abs(x-(left+right)/2) - ((right-left)/2 - radius)
		qy := math.Abs(y-(top+bottom)/2) - ((bottom-top)/2 - radius)
		return math.Hypot(math.Max(qx, 0), math.Max(qy, 0)) + math.Min(math.Max(qx, qy), 0) - radius
	}
	unit := func(v float64) float64 { return math.Min(math.Max(v, 0), 1) }
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			px, py := float64(x)+.5, float64(y)+.5
			d := dist(px, py)
			sheet := unit(.5 - d)
			fade := unit(1 - dist(px, py-3*s)/(7*s))
			shadow := th.shadow * fade * fade
			alpha := sheet + shadow*(1-sheet)
			if alpha <= 0 {
				continue
			}
			c := fill
			switch {
			case py < top+strip:
				c = stripColor
			case d > -border:
				c = edge
			}
			r, g, b := rgb(c)
			lit := sheet / alpha // the shadow is black, so it only darkens
			img.SetNRGBA(x, y, color.NRGBA{uint8(float64(r) * lit), uint8(float64(g) * lit), uint8(float64(b) * lit), uint8(alpha * opacity * 255)})
		}
	}
	var buf bytes.Buffer
	(&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&buf, img)
	return buf.Bytes()
}
