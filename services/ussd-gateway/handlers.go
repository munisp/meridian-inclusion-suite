package main

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/httpx"
)

// busAdapter adapts the platform inproc bus to the action eventPublisher.
type busAdapter struct{ bus events.Bus }

func (b busAdapter) Publish(topic string, data map[string]any) {
	_ = b.bus.Publish(topic, events.New(topic, serviceName, "", "", data))
}

type server struct {
	graph  *MenuGraph
	engine *Engine
	store  SessionStore
	bus    events.Bus
	mu     sync.Mutex // serialize per-session processing in dev
}

func (s *server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", httpx.Healthz(serviceName, serviceVersion))
	mux.HandleFunc("GET /readyz", httpx.Readyz(nil))
	mux.HandleFunc("POST /webhook/ussd", s.webhook)
	mux.HandleFunc("POST /v1/simulate", s.simulate)
	mux.HandleFunc("GET /v1/menus", s.menus)
	mux.HandleFunc("GET /v1/sessions/{id}", s.session)
	return mux
}

// processInput runs one input against the session (creating it on first use)
// and returns the USSD response text with CON/END prefix.
func (s *server) processInput(sessionID, phone, input string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.store.Get(sessionID)
	if !ok {
		sess = &Session{ID: sessionID, Phone: phone, Data: map[string]string{}, CreatedAt: time.Now()}
		s.store.Put(sess)
		text, cont, err := s.engine.Start(sess)
		s.store.Put(sess)
		if err != nil {
			return "END Service error. Please try again later."
		}
		if strings.TrimSpace(input) == "" {
			return prefix(text, cont)
		}
		// fall through: some networks deliver the first input immediately
		if !cont {
			return prefix(text, cont)
		}
	}
	text, cont, err := s.engine.Handle(sess, input)
	if err != nil {
		s.store.Delete(sessionID)
		return "END " + text
	}
	if !cont {
		s.store.Delete(sessionID)
	} else {
		s.store.Put(sess)
	}
	return prefix(text, cont)
}

func prefix(text string, cont bool) string {
	if cont {
		return "CON " + text
	}
	return "END " + text
}

// webhook implements the Africa's-Talking-style callback:
// form fields sessionId, serviceCode, phoneNumber, text (cumulative, '*'-
// separated). Responds text/plain with CON/END.
func (s *server) webhook(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_form", err.Error())
		return
	}
	sessionID := r.FormValue("sessionId")
	phone := r.FormValue("phoneNumber")
	text := r.FormValue("text")
	if sessionID == "" {
		httpx.WriteProblem(w, http.StatusBadRequest, "validation", "sessionId is required")
		return
	}
	// Replay the cumulative input chain: process only inputs not yet consumed.
	parts := []string{}
	if text != "" {
		parts = strings.Split(text, "*")
	}
	sess, ok := s.store.Get(sessionID)
	consumed := 0
	if ok {
		if v, has := sess.Data["__consumed"]; has {
			consumed, _ = strconv.Atoi(v)
		}
	}
	var resp string
	if len(parts) <= consumed {
		// nothing new: re-render current menu (empty input)
		resp = s.processInput(sessionID, phone, "")
		if resp == "CON Session error. Please redial." || len(parts) == 0 {
			// fresh session
			resp = s.processInput(sessionID, phone, "")
		}
	} else {
		for _, p := range parts[consumed:] {
			resp = s.processInput(sessionID, phone, p)
			consumed++
		}
		s.mu.Lock()
		if sess2, ok2 := s.store.Get(sessionID); ok2 {
			sess2.Data["__consumed"] = itoa(consumed)
			s.store.Put(sess2)
		}
		s.mu.Unlock()
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(resp))
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

// simulate runs a full session from a list of inputs — the built-in
// simulator for testing a full session via curl.
func (s *server) simulate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Phone  string   `json:"phone"`
		Inputs []string `json:"inputs"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if in.Phone == "" {
		in.Phone = "+2348000000000"
	}
	sessionID := "sim-" + itoa(int(time.Now().UnixNano()%1000000000))
	type step struct {
		Input    string `json:"input"`
		Response string `json:"response"`
	}
	transcript := []step{}
	resp := s.processInput(sessionID, in.Phone, "")
	transcript = append(transcript, step{Input: "(dial " + s.graph.ServiceCode + ")", Response: resp})
	for _, inp := range in.Inputs {
		resp = s.processInput(sessionID, in.Phone, inp)
		transcript = append(transcript, step{Input: inp, Response: resp})
		if strings.HasPrefix(resp, "END ") {
			break
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"session_id": sessionID, "phone": in.Phone, "transcript": transcript,
	})
}

func (s *server) menus(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, s.graph)
}

func (s *server) session(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.store.Get(r.PathValue("id"))
	if !ok {
		httpx.WriteProblem(w, http.StatusNotFound, "not_found", "session not found or expired")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, sess)
}
