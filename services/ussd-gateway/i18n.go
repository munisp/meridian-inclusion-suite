package main

import "strings"

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
