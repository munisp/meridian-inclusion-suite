package main

import (
	"os"
	"strings"
	"sync"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

// i18n.go — multilingual taxpayer comms (I15): template packs for USSD menus
// and SMS/receipt strings in English, Hausa, Yoruba, Igbo and Nigerian
// Pidgin, with per-taxpayer language preference.
//
// REAL: the engine renders these bundles when sess.Data["lang"] is set;
// callers dial "#ha"/"#yo"/"#ig"/"#pcm"/"#en" at any menu to switch, and the
// gateway greets returning MSISDNs in their stored language. SMS/receipt
// strings (sms_*) are used by the aggregator notifier and receipt flows.

// Lang is a supported taxpayer language code.
type Lang string

const (
	LangEN  Lang = "en"
	LangHA  Lang = "ha"  // Hausa
	LangYO  Lang = "yo"  // Yoruba
	LangIG  Lang = "ig"  // Igbo
	LangPCM Lang = "pcm" // Nigerian Pidgin
)

// bundleKeys are the string keys every language pack MUST define (tested for
// completeness). Keys are menu ids from menus.json plus sms_/receipt_ keys.
var bundleKeys = []string{
	"home", "onb_nin", "onb_name", "onb_done", "onb_error",
	"psm_state", "psm_trade", "lang_switched",
	"sms_receipt", "sms_welcome", "receipt_footer",
}

// bundles is the template pack registry: lang -> key -> text.
var bundles = map[Lang]map[string]string{
	LangEN: {
		"home":           "Welcome to NRS Tax Services\n1. Register (Onboarding)\n2. TIN status\n3. Pay presumptive levy",
		"onb_nin":        "Enter your 11-digit NIN:",
		"onb_name":       "Enter your full name:",
		"onb_done":       "Registration successful, {{name}}.\nYour TIN: {{tin}}\nStatus: {{status}}",
		"onb_error":      "Registration failed: {{error}}\nPlease try again or visit an NRS office.",
		"psm_state":      "Presumptive levy - select state:\n1. Lagos\n2. Kano\n3. Other (annual levy applies)",
		"psm_trade":      "Select your trade:\n1. Food vendor\n2. Tailoring\n3. Artisan\n4. Transport\n5. Retail",
		"lang_switched":  "Language set to English.",
		"sms_receipt":    "NRS receipt {{serial}}: {{amount}} received for {{purpose}}. Verify at nrs.gov.ng/verify",
		"sms_welcome":    "Welcome to NRS Tax Services. Your TIN: {{tin}}.",
		"receipt_footer": "Thank you for paying your tax. - NRS",
	},
	LangHA: {
		"home":           "Barka da zuwa NRS Tax Services\n1. Yi rajista\n2. Duba matsayin TIN\n3. Bada haraji",
		"onb_nin":        "Shigar da lambar NIN mai lamba 11:",
		"onb_name":       "Shigar da cikakken sunanka:",
		"onb_done":       "Rajista ya yi nasara, {{name}}.\nTIN naka: {{tin}}\nMatsayi: {{status}}",
		"onb_error":      "Rajista ya kasa: {{error}}\nSake gwadawa ko ziyarci ofishin NRS.",
		"psm_state":      "Haraji - zaɓi jiha:\n1. Lagos\n2. Kano\n3. Wata jiha",
		"psm_trade":      "Zaɓi sana'arka:\n1. Mai sayar da abinci\n2. ɗinki\n3. Sana'ar hannu\n4. Sufuri\n5. Kasuwanci",
		"lang_switched":  "An canza yare zuwa Hausa.",
		"sms_receipt":    "Rasit NRS {{serial}}: an karba {{amount}} don {{purpose}}. Tabbatar a nrs.gov.ng/verify",
		"sms_welcome":    "Barka da zuwa NRS. TIN naka: {{tin}}.",
		"receipt_footer": "Na gode da biyan haraji. - NRS",
	},
	LangYO: {
		"home":           "Kaabo si NRS Tax Services\n1. Forukọsilẹ\n2. Ṣayẹwo TIN\n3. San owo-ori",
		"onb_nin":        "Tẹ NIN ọna 11 rẹ sii:",
		"onb_name":       "Tẹ orukọ kikun rẹ sii:",
		"onb_done":       "Forukọsilẹ yọrisi rere, {{name}}.\nTIN rẹ: {{tin}}\nIpo: {{status}}",
		"onb_error":      "Forukọsilẹ kuna: {{error}}\nJọwọ tun gbiyanju tabi lọ si Ọfiisi NRS.",
		"psm_state":      "Owo-ori - yan ipinlẹ:\n1. Eko\n2. Kano\n3. Omiiran",
		"psm_trade":      "Yan iṣẹ rẹ:\n1. Onítańjẹ\n2. Aláṣọ\n3. Ọlọ́wọ́-iṣẹ́\n4. Ìrìnnàjò\n5. Òwò",
		"lang_switched":  "Ede ti yipada si Yoruba.",
		"sms_receipt":    "Erisi NRS {{serial}}: a gba {{amount}} fun {{purpose}}. Ṣayẹwo ni nrs.gov.ng/verify",
		"sms_welcome":    "Kaabo si NRS. TIN rẹ: {{tin}}.",
		"receipt_footer": "O ṣeun fun sisan owo-ori rẹ. - NRS",
	},
	LangIG: {
		"home":           "Nnọọ na NRS Tax Services\n1. Debanye aha\n2. Lelee TIN\n3. Kwụọ ụtụ",
		"onb_nin":        "Tinye NIN gị (ọnụ ọgụgụ 11):",
		"onb_name":       "Tinye aha gị zuru ezu:",
		"onb_done":       "Ndebanye aha gara nke ọma, {{name}}.\nTIN gị: {{tin}}\nỌnọdụ: {{status}}",
		"onb_error":      "Ndebanye aha dara ada: {{error}}\nBiko nwaa ọzọ ma ọ bụ gaa Ọfịs NRS.",
		"psm_state":      "Ụtụ - họrọ steeti:\n1. Lagos\n2. Kano\n3. Steeti ọzọ",
		"psm_trade":      "Họrọ ọrụ gị:\n1. Onye na-ere nri\n2. Onye na-akwa uwe\n3. Omenka\n4. Ụgbọ njem\n5. Ahia",
		"lang_switched":  "Asụsụ agbanwela na Igbo.",
		"sms_receipt":    "Nnata NRS {{serial}}: enatara {{amount}} maka {{purpose}}. Lelee na nrs.gov.ng/verify",
		"sms_welcome":    "Nnọọ na NRS. TIN gị: {{tin}}.",
		"receipt_footer": "Daalụ n'ihi ịkwụ ụtụ gị. - NRS",
	},
	LangPCM: {
		"home":           "Welcome to NRS Tax Services\n1. Register\n2. Check TIN\n3. Pay tax",
		"onb_nin":        "Abeg enter your 11-digit NIN:",
		"onb_name":       "Abeg enter your full name:",
		"onb_done":       "Registration don complete, {{name}}.\nYour TIN: {{tin}}\nStatus: {{status}}",
		"onb_error":      "Registration no work: {{error}}\nAbeg try again or go NRS office.",
		"psm_state":      "Tax - pick your state:\n1. Lagos\n2. Kano\n3. Another state",
		"psm_trade":      "Pick your work:\n1. Food seller\n2. Tailor\n3. Artisan\n4. Transport\n5. Market trade",
		"lang_switched":  "Language don change to Pidgin.",
		"sms_receipt":    "NRS receipt {{serial}}: dem don collect {{amount}} for {{purpose}}. Check am for nrs.gov.ng/verify",
		"sms_welcome":    "Welcome to NRS. Your TIN na: {{tin}}.",
		"receipt_footer": "Thank you for paying your tax. - NRS",
	},
}

// T renders a template key in the requested language, falling back to
// English and finally to the fallback text (the menus.json default).
func T(lang, key, fallback string) string {
	if b, ok := bundles[Lang(lang)]; ok {
		if t, ok := b[key]; ok {
			return t
		}
	}
	if t, ok := bundles[LangEN][key]; ok {
		return t
	}
	return fallback
}

// langFromDial parses a language-switch dial code ("#ha", "#yo", ...).
func langFromDial(input string) (Lang, bool) {
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "#en":
		return LangEN, true
	case "#ha":
		return LangHA, true
	case "#yo":
		return LangYO, true
	case "#ig":
		return LangIG, true
	case "#pcm", "#pidgin":
		return LangPCM, true
	}
	return "", false
}

// localizedText returns the menu text in the session's preferred language
// (I15), falling back to the menus.json English text for menus without a
// translated key.
func localizedText(sess *Session, menu Menu) string {
	lang := sess.Data["lang"]
	if lang == "" || lang == string(LangEN) {
		return menu.Text
	}
	return T(lang, sess.Menu, menu.Text)
}

// ---- Language pack framework (N2) ----
//
// Prompt-descriptor packs externalize the core-flow menus/prompts per
// locale (en, ha, yo, ig) with an explicit fallback chain (any locale ->
// en). Each prompt carries IVR rendering hooks: tts_text (speakable form)
// and audio_ref (pre-recorded prompt asset) so an IVR gateway can render
// voice. [IVR] — interface/hooks only: no IVR gateway is integrated here.
//
// Locale lifecycle: a first-time MSISDN is shown the injected "lang_select"
// menu (config flag USSD_LANG_SELECT=1; engine-level flows are unaffected
// when off), the choice is written to the session AND persisted per-MSISDN
// (LocaleStore: KV when sessions are KV-backed, in-mem otherwise), so the
// next dial greets in the stored language. Mid-session dial codes (#ha etc.,
// above) also persist.

// Prompt is one externalized prompt descriptor (USSD text + IVR hooks).
type Prompt struct {
	Text     string `json:"text"`                // USSD display text (menus.json content, ported)
	TTSText  string `json:"tts_text"`            // speakable form for IVR TTS [IVR]
	AudioRef string `json:"audio_ref,omitempty"` // pre-recorded asset, e.g. ivr://ha/home.wav [IVR]
}

// corePromptKeys are the menu/prompt ids of the core flows (registration,
// tin_status, payments menu, PIN flows) that every locale pack MUST define
// (completeness-tested).
var corePromptKeys = []string{
	// registration
	"home", "onb_nin", "onb_name", "onb_state", "onb_done", "onb_error",
	// tin status
	"onb_tin_input", "onb_tin_done",
	// payments menu
	"psm_state", "psm_trade", "psm_turnover", "psm_exempt",
	"psm_pay_confirm", "psm_cancel", "psm_pay_done", "psm_pay_error",
	// PIN flows
	"pin_setup_input", "pin_setup_confirm", "pin_verify_input", "pin_fail",
	// locale UX
	"lang_select", "lang_switched",
}

// promptFallback is the fallback chain: every non-English locale falls back
// to English for missing keys (single-hop; en is the root).
var promptFallback = map[Lang]Lang{
	LangHA: LangEN, LangYO: LangEN, LangIG: LangEN, LangPCM: LangEN,
}

// promptPacks is the locale pack registry: lang -> key -> Prompt. English
// texts are ported verbatim from menus.json; ha/yo/ig are translations of
// the core flows. Extra en-only keys (non-core flows, e.g. certificates)
// exercise the fallback chain.
var promptPacks = map[Lang]map[string]Prompt{
	LangEN: {
		"home":              {Text: "Welcome to NRS Tax Services\n1. Register (Onboarding)\n2. TIN provision status\n3. Pay presumptive levy\n4. Verify certificate"},
		"onb_nin":           {Text: "Enter your 11-digit NIN:"},
		"onb_name":          {Text: "Enter your full name:"},
		"onb_state":         {Text: "Select your state:\n1. Lagos\n2. Kano\n3. Other"},
		"onb_done":          {Text: "Registration successful, {{name}}.\nYour TIN: {{tin}}\nStatus: NIN verified, TIN provisioned.\nKeep your TIN safe."},
		"onb_error":         {Text: "Registration failed: {{error}}\nPlease try again or visit an NRS agent."},
		"onb_tin_input":     {Text: "Enter your 11-digit NIN to check TIN status:"},
		"onb_tin_done":      {Text: "TIN status for NIN {{nin_masked}}:\nTIN: {{tin}}\nStatus: {{tin_status}}"},
		"psm_state":         {Text: "Presumptive levy - select state:\n1. Lagos\n2. Kano\n3. Other (federal schedule)"},
		"psm_trade":         {Text: "Select your trade:\n1. Food vendor\n2. Tailoring\n3. Artisan\n4. Transport\n5. Retail\n6. Services"},
		"psm_turnover":      {Text: "Annual turnover:\n1. Up to N800,000\n2. N800,001 - N1,000,000\n3. N1m - N5m\n4. N5m - N25m"},
		"psm_exempt":        {Text: "{{error}}\nNo payment is due. Thank you."},
		"psm_pay_confirm":   {Text: "Band: {{band}}\nAnnual levy: N{{levy_naira}} (incl. admin fee N{{fee_naira}})\n1. Pay now\n2. Cancel"},
		"psm_cancel":        {Text: "Payment cancelled. Dial *347*88# anytime to restart."},
		"psm_pay_done":      {Text: "Payment of N{{levy_naira}} received (ref {{pssp_ref}}).\nCertificate: {{cert_serial}}\nYou will receive an SMS with your certificate serial.\nVerify at /v1/certificates/verify/{{cert_serial}}"},
		"psm_pay_error":     {Text: "Payment failed: {{error}}\nNo charge was made. Please try again."},
		"pin_setup_input":   {Text: "Create a 4-digit PIN to protect your tax records:"},
		"pin_setup_confirm": {Text: "Confirm your PIN:"},
		"pin_verify_input":  {Text: "Enter your PIN to continue:"},
		"pin_fail":          {Text: "{{error}}"},
		"lang_select":       {Text: "Choose language / Zaɓi yare / Yan ede / Họrọ asụsụ:\n1. English\n2. Hausa\n3. Yoruba\n4. Igbo"},
		"lang_switched":     {Text: "Language set to English."},
		// en-only (non-core) keys: fallback chain covers other locales
		"cert_input": {Text: "Enter your certificate serial number:"},
		"cert_done":  {Text: "Certificate {{serial}} is VALID.\n{{detail}}"},
		"cert_bad":   {Text: "Certificate not found or invalid. Check the serial and try again."},
	},
	LangHA: {
		"home":              {Text: "Barka da zuwa NRS Tax Services\n1. Yi rajista\n2. Duba matsayin TIN\n3. Bada haraji\n4. Tabbatar da takardar shaida"},
		"onb_nin":           {Text: "Shigar da lambar NIN mai lamba 11:"},
		"onb_name":          {Text: "Shigar da cikakken sunanka:"},
		"onb_state":         {Text: "Zaɓi jiharka:\n1. Lagos\n2. Kano\n3. Wata jiha"},
		"onb_done":          {Text: "Rajista ya yi nasara, {{name}}.\nTIN naka: {{tin}}\nMatsayi: an tabbatar da NIN, an samar da TIN.\nKiyaye TIN naka."},
		"onb_error":         {Text: "Rajista ya kasa: {{error}}\nSake gwadawa ko ziyarci wakilin NRS."},
		"onb_tin_input":     {Text: "Shigar da NIN mai lamba 11 don duba matsayin TIN:"},
		"onb_tin_done":      {Text: "Matsayin TIN na NIN {{nin_masked}}:\nTIN: {{tin}}\nMatsayi: {{tin_status}}"},
		"psm_state":         {Text: "Haraji - zaɓi jiha:\n1. Lagos\n2. Kano\n3. Wata jiha (tsarin tarayya)"},
		"psm_trade":         {Text: "Zaɓi sana'arka:\n1. Mai sayar da abinci\n2. ɗinki\n3. Sana'ar hannu\n4. Sufuri\n5. Kasuwanci\n6. Ayyuka"},
		"psm_turnover":      {Text: "Jimlar kasuwanci na shekara:\n1. Har N800,000\n2. N800,001 - N1,000,000\n3. N1m - N5m\n4. N5m - N25m"},
		"psm_exempt":        {Text: "{{error}}\nBabu haraji da za a biya. Na gode."},
		"psm_pay_confirm":   {Text: "Rukuni: {{band}}\nHarajin shekara: N{{levy_naira}} (da kudi N{{fee_naira}})\n1. Biya yanzu\n2. Soke"},
		"psm_cancel":        {Text: "An soke biya. Kira *347*88# a kowane lokaci don sake farawa."},
		"psm_pay_done":      {Text: "An karbi N{{levy_naira}} (ref {{pssp_ref}}).\nTakardar shaida: {{cert_serial}}\nZa ka karbi SMS da lambar takardar.\nTabbatar a /v1/certificates/verify/{{cert_serial}}"},
		"psm_pay_error":     {Text: "Biya ya kasa: {{error}}\nBa a cire kudi ba. Sake gwadawa."},
		"pin_setup_input":   {Text: "Ƙirƙiri PIN mai lamba 4 don kare bayananka:"},
		"pin_setup_confirm": {Text: "Tabbatar da PIN:"},
		"pin_verify_input":  {Text: "Shigar da PIN don ci gaba:"},
		"pin_fail":          {Text: "{{error}}"},
		"lang_select":       {Text: "Zaɓi yare / Choose language:\n1. English\n2. Hausa\n3. Yoruba\n4. Igbo"},
		"lang_switched":     {Text: "An canza yare zuwa Hausa."},
	},
	LangYO: {
		"home":              {Text: "Kaabo si NRS Tax Services\n1. Forukọsilẹ\n2. Ṣayẹwo TIN\n3. San owo-ori\n4. Ṣayẹwo iwe-ẹri"},
		"onb_nin":           {Text: "Tẹ NIN ọna 11 rẹ sii:"},
		"onb_name":          {Text: "Tẹ orukọ kikun rẹ sii:"},
		"onb_state":         {Text: "Yan ipinlẹ rẹ:\n1. Eko\n2. Kano\n3. Omiiran"},
		"onb_done":          {Text: "Forukọsilẹ yọrisi rere, {{name}}.\nTIN rẹ: {{tin}}\nIpo: a ti jẹrisi NIN, a ti pese TIN.\nPa TIN rẹ mọ."},
		"onb_error":         {Text: "Forukọsilẹ kuna: {{error}}\nJọwọ tun gbiyanju tabi lọ ri aṣoju NRS."},
		"onb_tin_input":     {Text: "Tẹ NIN ọna 11 rẹ sii lati ṣayẹwo ipo TIN:"},
		"onb_tin_done":      {Text: "Ipo TIN fun NIN {{nin_masked}}:\nTIN: {{tin}}\nIpo: {{tin_status}}"},
		"psm_state":         {Text: "Owo-ori - yan ipinlẹ:\n1. Eko\n2. Kano\n3. Omiiran (eto ilu)"},
		"psm_trade":         {Text: "Yan iṣẹ rẹ:\n1. Onítańjẹ\n2. Aláṣọ\n3. Ọlọ́wọ́-iṣẹ́\n4. Ìrìnnàjò\n5. Òwò\n6. Iṣẹ"},
		"psm_turnover":      {Text: "Iye owo ti odun:\n1. Titi di N800,000\n2. N800,001 - N1,000,000\n3. N1m - N5m\n4. N5m - N25m"},
		"psm_exempt":        {Text: "{{error}}\nKo si owo-ori lati san. O ṣeun."},
		"psm_pay_confirm":   {Text: "Ẹgbẹ: {{band}}\nOwo-ori odun: N{{levy_naira}} (pẹlu owo iṣẹ N{{fee_naira}})\n1. San bayi\n2. Fagilee"},
		"psm_cancel":        {Text: "A fagilee isanwo. Pe *347*88# nigbakugba lati tun bẹrẹ."},
		"psm_pay_done":      {Text: "A gba N{{levy_naira}} (ref {{pssp_ref}}).\nIwe-ẹri: {{cert_serial}}\nIwọ yoo gba SMS pẹlu serial iwe-ẹri rẹ.\nṢayẹwo ni /v1/certificates/verify/{{cert_serial}}"},
		"psm_pay_error":     {Text: "Isanwo kuna: {{error}}\nKo si yiyọ kuro. Jọwọ tun gbiyanju."},
		"pin_setup_input":   {Text: "Ṣẹda PIN ọna 4 lati daabobo igbasilẹ rẹ:"},
		"pin_setup_confirm": {Text: "Jẹrisi PIN rẹ:"},
		"pin_verify_input":  {Text: "Tẹ PIN rẹ sii lati tẹsiwaju:"},
		"pin_fail":          {Text: "{{error}}"},
		"lang_select":       {Text: "Yan ede / Choose language:\n1. English\n2. Hausa\n3. Yoruba\n4. Igbo"},
		"lang_switched":     {Text: "Ede ti yipada si Yoruba."},
	},
	LangIG: {
		"home":              {Text: "Nnọọ na NRS Tax Services\n1. Debanye aha\n2. Lelee TIN\n3. Kwụọ ụtụ\n4. Lelee asambodo"},
		"onb_nin":           {Text: "Tinye NIN gị (ọnụ ọgụgụ 11):"},
		"onb_name":          {Text: "Tinye aha gị zuru ezu:"},
		"onb_state":         {Text: "Họrọ steeti gị:\n1. Lagos\n2. Kano\n3. Steeti ọzọ"},
		"onb_done":          {Text: "Ndebanye aha gara nke ọma, {{name}}.\nTIN gị: {{tin}}\nỌnọdụ: ekwenyela NIN, enyela TIN.\nChekwaa TIN gị."},
		"onb_error":         {Text: "Ndebanye aha dara ada: {{error}}\nBiko nwaa ọzọ ma ọ bụ hụ onye nnọchi NRS."},
		"onb_tin_input":     {Text: "Tinye NIN gị (ọnụ ọgụgụ 11) iji lelee ọnọdụ TIN:"},
		"onb_tin_done":      {Text: "Ọnọdụ TIN maka NIN {{nin_masked}}:\nTIN: {{tin}}\nỌnọdụ: {{tin_status}}"},
		"psm_state":         {Text: "Ụtụ - họrọ steeti:\n1. Lagos\n2. Kano\n3. Steeti ọzọ (usoro etiti)"},
		"psm_trade":         {Text: "Họrọ ọrụ gị:\n1. Onye na-ere nri\n2. Onye na-akwa uwe\n3. Omenka\n4. Ụgbọ njem\n5. Ahia\n6. Ọrụ"},
		"psm_turnover":      {Text: "Ngụkọta ahia kwa afọ:\n1. Ruo N800,000\n2. N800,001 - N1,000,000\n3. N1m - N5m\n4. N5m - N25m"},
		"psm_exempt":        {Text: "{{error}}\nỌ dịghị ụtụ ị ga-akwụ. Daalụ."},
		"psm_pay_confirm":   {Text: "Otu: {{band}}\nỤtụ kwa afọ: N{{levy_naira}} (tinyere ụgwọ N{{fee_naira}})\n1. Kwụọ ugbu a\n2. Kagbuo"},
		"psm_cancel":        {Text: "Akagbuola ịkwụ ụgwọ. Kpọọ *347*88# oge ọ bụla iji malite ọzọ."},
		"psm_pay_done":      {Text: "Enatara N{{levy_naira}} (ref {{pssp_ref}}).\nAsambodo: {{cert_serial}}\nỊ ga-enweta SMS nwere nọmba asambodo gị.\nLelee na /v1/certificates/verify/{{cert_serial}}"},
		"psm_pay_error":     {Text: "Ịkwụ ụgwọ dara ada: {{error}}\nA naghị ewepụ ego. Biko nwaa ọzọ."},
		"pin_setup_input":   {Text: "Mepụta PIN onu ogugu 4 iji chekwaa ndekọ gị:"},
		"pin_setup_confirm": {Text: "Kwenye PIN gị:"},
		"pin_verify_input":  {Text: "Tinye PIN gị iji gaa n'ihu:"},
		"pin_fail":          {Text: "{{error}}"},
		"lang_select":       {Text: "Họrọ asụsụ / Choose language:\n1. English\n2. Hausa\n3. Yoruba\n4. Igbo"},
		"lang_switched":     {Text: "Asụsụ agbanwela na Igbo."},
	},
}

// init fills derived IVR fields: tts_text defaults to the flattened USSD
// text (voice artists may override per key), audio_ref defaults to the
// conventional asset path ivr://<lang>/<key>.wav. [IVR]
func init() {
	for lang, pack := range promptPacks {
		for key, p := range pack {
			if p.TTSText == "" {
				p.TTSText = strings.Join(strings.Fields(strings.ReplaceAll(p.Text, "\n", " ")), " ")
			}
			if p.AudioRef == "" {
				p.AudioRef = "ivr://" + string(lang) + "/" + key + ".wav"
			}
			pack[key] = p
		}
	}
}

// P resolves a prompt descriptor for a locale, following the fallback chain
// (locale -> en). Returns ok=false when the key is unknown everywhere.
func P(lang, key string) (Prompt, bool) {
	l := Lang(lang)
	if pack, ok := promptPacks[l]; ok {
		if p, ok := pack[key]; ok {
			return p, true
		}
	}
	if fb, ok := promptFallback[l]; ok {
		if p, ok := promptPacks[fb][key]; ok {
			return p, true
		}
	}
	if p, ok := promptPacks[LangEN][key]; ok {
		return p, true
	}
	return Prompt{}, false
}

// IVRRenderer is the voice-render hook an IVR gateway implements: given a
// prompt descriptor it returns the TTS text and/or a pre-recorded audio
// asset to play. [IVR] — interface only, honestly tagged: no IVR gateway
// integration ships in this change.
type IVRRenderer interface {
	RenderPrompt(p Prompt) (ttsText, audioRef string)
}

// TTSFirstRenderer is the default [IVR] rendering policy: prefer the
// pre-recorded asset when present, always provide tts_text as fallback.
type TTSFirstRenderer struct{}

func (TTSFirstRenderer) RenderPrompt(p Prompt) (string, string) {
	return p.TTSText, p.AudioRef
}

// ---- locale persistence ----

// LocaleStore persists the per-MSISDN language preference across sessions.
type LocaleStore interface {
	Load(phone string) (string, bool)
	Save(phone, lang string)
}

// InMemLocaleStore is the dev in-memory locale store.
type InMemLocaleStore struct {
	mu sync.Mutex
	m  map[string]string
}

func NewInMemLocaleStore() *InMemLocaleStore { return &InMemLocaleStore{m: map[string]string{}} }

func (s *InMemLocaleStore) Load(phone string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[phone]
	return v, ok
}

func (s *InMemLocaleStore) Save(phone, lang string) {
	if phone == "" || lang == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[phone] = lang
}

// KVLocaleStore is the durable locale store (embedded KV; survives restarts).
type KVLocaleStore struct{ st *store.Store }

func NewKVLocaleStore(st *store.Store) *KVLocaleStore { return &KVLocaleStore{st: st} }

func (s *KVLocaleStore) Load(phone string) (string, bool) {
	var e struct {
		Lang string `json:"lang"`
	}
	ok, err := s.st.Get("ussd_locales", phone, &e)
	if err != nil || !ok {
		return "", false
	}
	return e.Lang, true
}

func (s *KVLocaleStore) Save(phone, lang string) {
	if phone == "" || lang == "" {
		return
	}
	_ = s.st.Put("ussd_locales", phone, map[string]string{"lang": lang})
}

// shared dev locale store (used when sessions are not KV-backed).
var devLocales = NewInMemLocaleStore()

// localeStore derives the LocaleStore from the session store: durable KV
// when sessions are KV-backed, the shared in-mem store otherwise.
func (s *server) localeStore() LocaleStore {
	if kv, ok := s.store.(*KVSessionStore); ok {
		return NewKVLocaleStore(kv.st)
	}
	return devLocales
}

// langSelectEnabled reports whether first-interaction language selection is
// active (config flag USSD_LANG_SELECT=1|true). Default off so engine-level
// flows/tests are unaffected.
func langSelectEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("USSD_LANG_SELECT")))
	return v == "1" || v == "true" || v == "yes"
}

// ensureLangSelectMenu injects the "lang_select" menu (first-interaction
// language picker) into the graph. Each option sets sess lang and continues
// to home.
func ensureLangSelectMenu(g *MenuGraph) {
	if _, ok := g.Menus["lang_select"]; ok {
		return
	}
	text, _ := P(string(LangEN), "lang_select")
	g.Menus["lang_select"] = Menu{
		Type: "options",
		Text: text.Text,
		Options: []MenuOption{
			{Key: "1", Next: "home", Set: map[string]string{"lang": string(LangEN)}},
			{Key: "2", Next: "home", Set: map[string]string{"lang": string(LangHA)}},
			{Key: "3", Next: "home", Set: map[string]string{"lang": string(LangYO)}},
			{Key: "4", Next: "home", Set: map[string]string{"lang": string(LangIG)}},
		},
	}
}
