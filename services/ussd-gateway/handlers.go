package main

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/httpx"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/keyx"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/webhookguard"
)

// busAdapter adapts the platform inproc bus to the action eventPublisher.
type busAdapter struct{ bus events.Bus }

func (b busAdapter) Publish(topic string, data map[string]any) {
	_ = b.bus.Publish(topic, events.New(topic, serviceName, "", "", data))
}

type server struct {
	graph    *MenuGraph
	engine   *Engine
	store    SessionStore
	bus      events.Bus
	notifier *AggregatorNotifier // nil in dev (USSD_AGGREGATOR_URL unset)
	mu       sync.Mutex          // serialize per-session processing in dev
	// guard is the webhook replay guard (M-1): X-Aggregator-Timestamp
	// within ±5 min + X-Aggregator-Nonce replay cache. Nil-safe: when nil
	// the check is skipped (unit tests constructing bare servers).
	guard *webhookguard.Guard
}

// checkReplay applies the webhook replay guard when configured: replays
// dedup to 409; stale/malformed/missing (prod) timestamps reject 401.
func (s *server) checkReplay(w http.ResponseWriter, r *http.Request) bool {
	if s.guard == nil {
		return true
	}
	err := s.guard.Check(r)
	if err == nil {
		return true
	}
	if errors.Is(err, webhookguard.ErrReplay) {
		httpx.WriteProblem(w, http.StatusConflict, "replay", "duplicate webhook delivery (nonce already seen)")
		return false
	}
	httpx.WriteProblem(w, http.StatusUnauthorized, "unauthorized", err.Error())
	return false
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
		// Resume handling (audit fix #8): a redial from the same MSISDN with a
		// still-live prior session gets a "continue?" prompt instead of a cold
		// restart. The special __resume menu is handled below.
		if idx, isIdx := s.store.(PhoneIndexer); isIdx {
			if prev, found := idx.GetByPhone(phone); found && prev.ID != sessionID && prev.Menu != "" {
				resume := &Session{
					ID: sessionID, Phone: phone, Menu: "__resume",
					Data:      map[string]string{"__resume_target": prev.ID},
					CreatedAt: time.Now(),
				}
				s.store.Put(resume)
				return "CON Network interruption? 1. Continue last transaction 2. Start over"
			}
		}
		sess = &Session{ID: sessionID, Phone: phone, Data: map[string]string{}, CreatedAt: time.Now()}
		// N2: first-interaction language selection. A returning MSISDN gets
		// its persisted locale applied; a first-time MSISDN (flag
		// USSD_LANG_SELECT=1) picks a language before the start menu.
		if s.startWithLocale(sess) {
			s.store.Put(sess)
			return "CON " + render(s.graph.Menus["lang_select"].Text, sess)
		}
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
	// Resume prompt dispatch: adopt the prior session's state (re-keyed to the
	// new sessionId) or discard it and start over.
	if sess.Menu == "__resume" {
		targetID := sess.Data["__resume_target"]
		s.store.Delete(sessionID)
		if strings.TrimSpace(input) == "1" {
			prev, found := s.store.Get(targetID)
			if !found {
				ns := &Session{ID: sessionID, Phone: phone, Data: map[string]string{}, CreatedAt: time.Now()}
				s.store.Put(ns)
				text, cont, err := s.engine.Start(ns)
				s.store.Put(ns)
				if err != nil {
					return "END Service error. Please try again later."
				}
				return prefix(text, cont)
			}
			prev.ID = sessionID // re-key to the new session id
			s.store.Delete(targetID)
			s.store.Put(prev)
			m := s.graph.Menus[prev.Menu]
			return "CON Resuming. " + render(m.Text, prev)
		}
		s.store.Delete(targetID)
		ns := &Session{ID: sessionID, Phone: phone, Data: map[string]string{}, CreatedAt: time.Now()}
		s.store.Put(ns)
		text, cont, err := s.engine.Start(ns)
		s.store.Put(ns)
		if err != nil {
			return "END Service error. Please try again later."
		}
		return prefix(text, cont)
	}

	// I15: language switching by dial code (#en #ha #yo #ig #pcm) at any menu;
	// the preference lives in the session (per-taxpayer) and is echoed in
	// the switched-to language.
	if lang, ok := langFromDial(input); ok {
		sess.Data["lang"] = string(lang)
		s.localeStore().Save(phone, string(lang)) // N2: persist per-MSISDN
		s.store.Put(sess)
		m := s.graph.Menus[sess.Menu]
		return "CON " + T(string(lang), "lang_switched", "") + " " + render(localizedText(sess, m), sess)
	}

	text, cont, err := s.engine.Handle(sess, input)
	if err != nil {
		s.store.Delete(sessionID)
		return "END " + text
	}
	if !cont {
		s.store.Delete(sessionID)
	} else {
		// N2: a language picked at lang_select (or otherwise set mid-flow)
		// persists for the MSISDN's next session.
		if langSelectEnabled() && sess.Data["lang"] != "" {
			s.localeStore().Save(phone, sess.Data["lang"])
		}
		s.store.Put(sess)
	}
	return prefix(text, cont)
}

// startWithLocale applies the persisted MSISDN locale to a fresh session,
// or (first-time MSISDN, USSD_LANG_SELECT=1) redirects to the injected
// lang_select menu. Returns true when the caller should render lang_select
// instead of engine.Start.
func (s *server) startWithLocale(sess *Session) bool {
	if !langSelectEnabled() {
		return false
	}
	ensureLangSelectMenu(s.graph)
	if lang, ok := s.localeStore().Load(sess.Phone); ok {
		sess.Data["lang"] = lang
		return false
	}
	if sess.Phone == "" {
		return false
	}
	sess.Menu = "lang_select" // engine.Handle consumes the pick from here
	return true
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
	// Read the raw body first for aggregator HMAC verification (H4).
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if !VerifyAggregatorSignature(r.Header.Get("X-Aggregator-Signature"), raw) {
		httpx.WriteProblem(w, http.StatusUnauthorized, "unauthorized", "invalid aggregator signature")
		return
	}
	// M-1: timestamp tolerance + replay protection on top of the HMAC.
	if !s.checkReplay(w, r) {
		return
	}
	form, err := url.ParseQuery(string(raw))
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_form", err.Error())
		return
	}
	sessionID := form.Get("sessionId")
	phone := form.Get("phoneNumber")
	text := form.Get("text")
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
	// Outbound aggregator notify on session end (prod profile only).
	if s.notifier != nil && strings.HasPrefix(resp, "END ") {
		go func() { _ = s.notifier.Notify(sessionID, phone, resp) }()
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
	// F-3: fail closed in prod — the simulator drives the real menu engine and
	// action bus (menu/action enumeration + bus events). Even an authenticated
	// prod caller gets 404; see publicPath in main.go for the auth gate.
	if keyx.Prod() {
		httpx.WriteProblem(w, http.StatusNotFound, "not_found", "unknown route")
		return
	}
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
