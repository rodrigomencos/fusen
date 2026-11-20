package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed ui/index.html
var indexHTML []byte

type server struct {
	store Store

	mu    sync.Mutex // serializes reads and writes of the board through the UI
	lists []string   // files behind the notes' wedo checklists, as of the last read

	subMu sync.Mutex
	subs  map[chan struct{}]struct{}
}

type boardState struct {
	Name   string          `json:"name"`
	Root   string          `json:"root"`
	Notes  []Note          `json:"notes"`
	Layout map[string]Rect `json:"layout"`
}

type noteInput struct {
	Title  *string   `json:"title"`
	Body   *string   `json:"body"`
	Labels *[]string `json:"labels"`
	Color  *string   `json:"color"`
	Wedo   *string   `json:"wedo"`
	Task   *string   `json:"task"`
	Rev    string    `json:"rev"`
	X      *float64  `json:"x"`
	Y      *float64  `json:"y"`
}

func serve(store Store, ln net.Listener) error {
	s := &server{store: store, subs: map[chan struct{}]struct{}{}}
	go s.watch()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/board", s.getBoard)
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("POST /api/notes", s.createNote)
	mux.HandleFunc("PUT /api/notes/{id}", s.updateNote)
	mux.HandleFunc("DELETE /api/notes/{id}", s.deleteNote)
	mux.HandleFunc("PUT /api/layout/{id}", s.moveNote)
	mux.HandleFunc("POST /api/notes/{id}/wedo", s.tickItem)
	mux.HandleFunc("POST /api/notes/{id}/reply", s.replyNote)
	return http.Serve(ln, s.guard(mux))
}

// guard keeps other sites from reaching the board through the browser: the
// request has to be addressed to this machine and, when a page sent it, come
// from the board's own page.
func (s *server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		origin := r.Header.Get("Origin")
		if (host != "127.0.0.1" && host != "localhost") || (origin != "" && origin != "http://"+r.Host) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		next.ServeHTTP(w, r)
	})
}

// watch tells connected pages when files change, whoever changed them.
func (s *server) watch() {
	signature := func() string {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.store.Signature(s.lists)
	}
	last := signature()
	for range time.Tick(500 * time.Millisecond) {
		if sig := signature(); sig != last {
			last = sig
			s.store.Sync()
			s.subMu.Lock()
			for ch := range s.subs {
				select {
				case ch <- struct{}{}:
				default:
				}
			}
			s.subMu.Unlock()
		}
	}
}

func (s *server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch := make(chan struct{}, 1)
	s.subMu.Lock()
	s.subs[ch] = struct{}{}
	s.subMu.Unlock()
	defer func() {
		s.subMu.Lock()
		delete(s.subs, ch)
		s.subMu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	keepAlive := time.NewTicker(25 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			fmt.Fprint(w, "data: change\n\n")
		case <-keepAlive.C:
			fmt.Fprint(w, ": ping\n\n")
		}
		flusher.Flush()
	}
}

func (s *server) getBoard(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	notes, layout, err := s.store.Board(os.Getenv("USER"))
	if err != nil {
		fail(w, err)
		return
	}
	root := s.store.root()
	s.lists = wedoFiles(root, notes)
	reply(w, http.StatusOK, boardState{Name: filepath.Base(root), Root: root, Notes: notes, Layout: layout})
}

func (s *server) createNote(w http.ResponseWriter, r *http.Request) {
	var in noteInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := Note{Title: "Untitled", By: os.Getenv("USER"), Created: time.Now()}
	in.applyTo(&n)
	id, err := s.store.NewID(n.Title)
	if err != nil {
		fail(w, err)
		return
	}
	n.ID = id
	saved, err := s.store.Save(n)
	if err != nil {
		fail(w, err)
		return
	}
	if in.X != nil && in.Y != nil {
		layout, err := s.store.Layout()
		if err != nil {
			fail(w, err)
			return
		}
		layout[id] = Rect{X: *in.X, Y: *in.Y, W: 300, H: 220}
		if err := s.store.SaveLayout(layout); err != nil {
			fail(w, err)
			return
		}
	}
	s.show(&saved)
	reply(w, http.StatusCreated, saved)
}

func (s *server) updateNote(w http.ResponseWriter, r *http.Request) {
	var in noteInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// The page sends the revision it started from, so an edit never silently
	// replaces what an agent wrote in the meantime.
	var conflict *Note
	saved, err := s.store.change(r.PathValue("id"), func(n *Note, _ []Note) error {
		if in.Rev != "" && in.Rev != n.Rev {
			conflict = n
			return errors.New("conflict")
		}
		in.applyTo(n)
		n.By = os.Getenv("USER")
		return nil
	})
	switch {
	case conflict != nil:
		reply(w, http.StatusConflict, conflict)
	case err != nil:
		fail(w, err)
	default:
		s.show(&saved)
		reply(w, http.StatusOK, saved)
	}
}

// replyNote adds the person's reply under a note, which also answers a task blocked on a question.
func (s *server) replyNote(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Text) == "" {
		http.Error(w, "a reply needs text", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.store.change(r.PathValue("id"), func(n *Note, _ []Note) error {
		answer(n, os.Getenv("USER"), in.Text)
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// show fills in a saved note the way the board sends it, so the page can paint it without a reload.
func (s *server) show(n *Note) {
	notes, _ := s.store.List()
	s.store.view(n, notes, os.Getenv("USER"))
}

func (s *server) deleteNote(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.store.Delete(r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) moveNote(w http.ResponseWriter, r *http.Request) {
	var rect Rect
	if err := json.NewDecoder(r.Body).Decode(&rect); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	layout, err := s.store.Layout()
	if err != nil {
		fail(w, err)
		return
	}
	layout[r.PathValue("id")] = rect
	if err := s.store.SaveLayout(layout); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// tickItem checks or unchecks one item of the wedo list a note shows.
func (s *server) tickItem(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Item int  `json:"item"`
		Done bool `json:"done"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err := s.store.Get(r.PathValue("id"))
	if err == nil {
		err = wedoSet(s.store.root(), n.Wedo, in.Item, in.Done)
	}
	if err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (in noteInput) applyTo(n *Note) {
	if in.Title != nil {
		n.Title = *in.Title
	}
	if in.Body != nil {
		n.Body = *in.Body
	}
	if in.Labels != nil {
		n.Labels = *in.Labels
	}
	if in.Color != nil {
		n.Color = *in.Color
	}
	if in.Wedo != nil {
		n.Wedo = *in.Wedo
	}
	if in.Task != nil && *in.Task != n.Task {
		settle(n, n.By, *in.Task)
	}
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, os.ErrNotExist) {
		status = http.StatusNotFound
	}
	http.Error(w, err.Error(), status)
}
