package main

// langpack_test.go — N2 coverage: pack completeness across en/ha/yo/ig,
// fallback chain, first-interaction locale selection + persistence, IVR
// prompt descriptor hooks, KV locale store.

import (
	"strings"
	"testing"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

// 1. All four packs define every core-flow key (registration, tin_status,
// payments menu, PIN flows) with USSD text and an IVR tts_text.
func TestCorePromptCompletenessAllPacks(t *testing.T) {
	for _, lang := range []Lang{LangEN, LangHA, LangYO, LangIG} {
		pack, ok := promptPacks[lang]
		if !ok {
			t.Fatalf("pack %s missing", lang)
		}
		for _, key := range corePromptKeys {
			p, ok := pack[key]
			if !ok {
				t.Fatalf("pack %s missing core key %s", lang, key)
			}
			if p.Text == "" || p.TTSText == "" {
				t.Fatalf("pack %s key %s: empty text/tts", lang, key)
			}
		}
	}
}

// 2. Placeholder variables stay intact across translations (render() relies
// on them) for the templated core keys.
func TestTranslatedPlaceholdersPreserved(t *testing.T) {
	templated := map[string][]string{
		"onb_done":        {"{{name}}", "{{tin}}"},
		"onb_tin_done":    {"{{nin_masked}}", "{{tin}}", "{{tin_status}}"},
		"psm_pay_confirm": {"{{band}}", "{{levy_naira}}", "{{fee_naira}}"},
		"psm_pay_done":    {"{{levy_naira}}", "{{pssp_ref}}", "{{cert_serial}}"},
	}
	for _, lang := range []Lang{LangHA, LangYO, LangIG} {
		for key, vars := range templated {
			for _, v := range vars {
				if !strings.Contains(promptPacks[lang][key].Text, v) {
					t.Fatalf("%s/%s lost placeholder %s", lang, key, v)
				}
			}
		}
	}
}

// 3. Fallback chain: a key absent from ha/yo/ig resolves to the en prompt;
// a key unknown everywhere returns ok=false.
func TestPromptFallbackChain(t *testing.T) {
	for _, lang := range []string{"ha", "yo", "ig"} {
		p, ok := P(lang, "cert_input") // en-only key
		if !ok || p.Text != promptPacks[LangEN]["cert_input"].Text {
			t.Fatalf("lang %s did not fall back to en: %+v ok=%v", lang, p, ok)
		}
	}
	if _, ok := P("yo", "definitely_not_a_key"); ok {
		t.Fatal("unknown key must return ok=false")
	}
	// en root has no fallback hop and still resolves
	if _, ok := P("en", "home"); !ok {
		t.Fatal("en/home must resolve")
	}
}

// 4. IVR hooks: every prompt carries a tts_text and a conventional
// audio_ref, and the [IVR] renderer surfaces them.
func TestIVRPromptDescriptors(t *testing.T) {
	p, ok := P("ha", "home")
	if !ok {
		t.Fatal("ha/home missing")
	}
	if p.AudioRef != "ivr://ha/home.wav" {
		t.Fatalf("audio_ref: %q", p.AudioRef)
	}
	if !strings.Contains(p.TTSText, "Barka") || strings.Contains(p.TTSText, "\n") {
		t.Fatalf("tts_text must be speakable single-line: %q", p.TTSText)
	}
	var r IVRRenderer = TTSFirstRenderer{}
	tts, audio := r.RenderPrompt(p)
	if tts == "" || audio == "" {
		t.Fatalf("renderer returned empty hooks: %q %q", tts, audio)
	}
}

// 5. First interaction (flag on): a new MSISDN gets the language picker
// before the start menu; picking Hausa renders the Hausa home menu.
func TestFirstInteractionLanguageSelection(t *testing.T) {
	t.Setenv("USSD_LANG_SELECT", "1")
	graph, _ := LoadMenuGraph()
	srv := &server{graph: graph, engine: NewEngine(graph, RegisterActions(&memPub{})), store: NewInMemSessionStore(180)}
	phone := "+2348060606001"

	r := srv.processInput("n2-a", phone, "")
	if !strings.HasPrefix(r, "CON ") || !strings.Contains(r, "Hausa") || strings.Contains(r, "Welcome to NRS Tax") {
		t.Fatalf("expected language picker first, got %q", r)
	}
	r = srv.processInput("n2-a", phone, "2") // Hausa
	if !strings.Contains(r, "Barka da zuwa") {
		t.Fatalf("expected Hausa home after pick, got %q", r)
	}
	sess, ok := srv.store.Get("n2-a")
	if !ok || sess.Data["lang"] != "ha" {
		t.Fatalf("session locale not set: %+v", sess)
	}
}

// 6. Locale persists across sessions: a redial (new session id) from the
// same MSISDN starts directly in the stored language — no picker.
func TestLocalePersistedAcrossSessions(t *testing.T) {
	t.Setenv("USSD_LANG_SELECT", "1")
	graph, _ := LoadMenuGraph()
	srv := &server{graph: graph, engine: NewEngine(graph, RegisterActions(&memPub{})), store: NewInMemSessionStore(180)}
	phone := "+2348060606002"

	srv.processInput("n2-b", phone, "")
	srv.processInput("n2-b", phone, "3") // Yoruba
	srv.store.Delete("n2-b")

	r := srv.processInput("n2-c", phone, "")
	if !strings.Contains(r, "Kaabo si NRS") {
		t.Fatalf("expected persisted Yoruba greeting, got %q", r)
	}
	if strings.Contains(r, "Choose language") {
		t.Fatalf("returning MSISDN must not see the picker: %q", r)
	}
}

// 7. Mid-session dial switch (#ig) persists too.
func TestDialSwitchPersists(t *testing.T) {
	t.Setenv("USSD_LANG_SELECT", "1")
	graph, _ := LoadMenuGraph()
	srv := &server{graph: graph, engine: NewEngine(graph, RegisterActions(&memPub{})), store: NewInMemSessionStore(180)}
	phone := "+2348060606003"

	srv.processInput("n2-d", phone, "")
	srv.processInput("n2-d", phone, "1") // English first
	srv.store.Delete("n2-d")
	r := srv.processInput("n2-e", phone, "")
	if !strings.Contains(r, "Welcome to NRS") {
		t.Fatalf("expected English, got %q", r)
	}
	r = srv.processInput("n2-e", phone, "#ig")
	if !strings.Contains(r, "Igbo") {
		t.Fatalf("expected Igbo switch, got %q", r)
	}
	srv.store.Delete("n2-e")
	r = srv.processInput("n2-f", phone, "")
	if !strings.Contains(r, "Nnọọ na NRS") {
		t.Fatalf("expected persisted Igbo greeting, got %q", r)
	}
}

// 8. KV-backed locale store round-trips and is what KV sessions derive.
func TestKVLocaleStoreRoundTrip(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/locales.json")
	if err != nil {
		t.Fatal(err)
	}
	ls := NewKVLocaleStore(st)
	if _, ok := ls.Load("+2348000000001"); ok {
		t.Fatal("unexpected hit")
	}
	ls.Save("+2348000000001", "yo")
	if got, ok := ls.Load("+2348000000001"); !ok || got != "yo" {
		t.Fatalf("round trip: %q ok=%v", got, ok)
	}
	// server derives the KV store from a KV session store
	srv := &server{store: NewKVSessionStore(st, 180)}
	if _, ok := srv.localeStore().(*KVLocaleStore); !ok {
		t.Fatalf("expected KVLocaleStore, got %T", srv.localeStore())
	}
}

// 9. The injected lang_select menu is wired: options set the session locale
// and continue to home.
func TestLangSelectMenuWiring(t *testing.T) {
	graph, _ := LoadMenuGraph()
	ensureLangSelectMenu(graph)
	m, ok := graph.Menus["lang_select"]
	if !ok {
		t.Fatal("menu not injected")
	}
	if m.Type != "options" || len(m.Options) != 4 {
		t.Fatalf("bad lang_select menu: %+v", m)
	}
	want := map[string]string{"1": "en", "2": "ha", "3": "yo", "4": "ig"}
	for _, o := range m.Options {
		if o.Next != "home" || o.Set["lang"] != want[o.Key] {
			t.Fatalf("option %s miswired: %+v", o.Key, o)
		}
	}
	// idempotent
	ensureLangSelectMenu(graph)
	if len(graph.Menus["lang_select"].Options) != 4 {
		t.Fatal("injection must be idempotent")
	}
}

// 10. Engine renders a localized core-flow menu from the legacy bundles
// (existing behavior preserved) and the new packs agree on the home menu.
func TestPacksConsistentWithLegacyBundles(t *testing.T) {
	for _, lang := range []Lang{LangEN, LangHA, LangYO, LangIG} {
		legacy := bundles[lang]["onb_nin"]
		pack := promptPacks[lang]["onb_nin"].Text
		if legacy != pack {
			t.Fatalf("%s onb_nin drift: bundle=%q pack=%q", lang, legacy, pack)
		}
	}
	sess := &Session{Data: map[string]string{"lang": "yo"}, CreatedAt: time.Now()}
	sess.Menu = "home"
	out := localizedText(sess, Menu{Text: promptPacks[LangEN]["home"].Text})
	if !strings.Contains(out, "Kaabo") {
		t.Fatalf("expected Yoruba render, got %q", out)
	}
}
