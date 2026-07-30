package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

//go:embed packs/*.json
var packFS embed.FS

// Pack is the JSON mirror of the §1.4 rp-* YAML format (embedded fallback
// copies; canonical YAML lives in the meridian-rule-packs repo).
type Pack struct {
	ID                 string         `json:"id"`
	Version            string         `json:"version"`
	EffectiveFrom      string         `json:"effective_from"`
	EffectiveTo        *string        `json:"effective_to"`
	Status             string         `json:"status"`
	SubjectToRegazette bool           `json:"subject_to_regazette"`
	Provenance         map[string]any `json:"provenance"`
	Rules              map[string]any `json:"rules"`
}

// TurnoverBand is one rp-turnover-bands row.
type TurnoverBand struct {
	ID       string  `json:"id"`
	Label    string  `json:"label"`
	MinKobo  uint64  `json:"min_kobo"`
	MaxKobo  *uint64 `json:"max_kobo"`
	Graduate bool    `json:"graduate"`
}

// BandEngine evaluates the presumptive packs.
type BandEngine struct {
	packs      map[string]Pack // id -> pack
	bands      []TurnoverBand
	exemptions []exemptionRule
}

type exemptionRule struct {
	ID                      string
	AnnualTurnoverBelowKobo uint64
	FixedAssetsBelowKobo    uint64
	IsCompany               bool
	Reason                  string
}

// LoadBandEngine loads the embedded fallback packs:
// rp-presumptive-federal / rp-presumptive-lagos / rp-presumptive-kano +
// rp-turnover-bands + rp-exemption-nta.
func LoadBandEngine() (*BandEngine, error) {
	e := &BandEngine{packs: map[string]Pack{}}
	entries, err := packFS.ReadDir("packs")
	if err != nil {
		return nil, err
	}
	for _, ent := range entries {
		b, err := packFS.ReadFile("packs/" + ent.Name())
		if err != nil {
			return nil, err
		}
		var p Pack
		if err := json.Unmarshal(b, &p); err != nil {
			return nil, fmt.Errorf("pack %s: %w", ent.Name(), err)
		}
		e.packs[p.ID] = p
	}
	// turnover bands
	tb, ok := e.packs["rp-turnover-bands"]
	if !ok {
		return nil, fmt.Errorf("rp-turnover-bands missing")
	}
	rawBands, _ := json.Marshal(tb.Rules["bands"])
	if err := json.Unmarshal(rawBands, &e.bands); err != nil {
		return nil, fmt.Errorf("rp-turnover-bands parse: %w", err)
	}
	sort.Slice(e.bands, func(i, j int) bool { return e.bands[i].MinKobo < e.bands[j].MinKobo })
	// exemptions
	if ex, ok := e.packs["rp-exemption-nta"]; ok {
		var rows []map[string]any
		raw, _ := json.Marshal(ex.Rules["exemptions"])
		if err := json.Unmarshal(raw, &rows); err == nil {
			for _, row := range rows {
				when, _ := row["when"].(map[string]any)
				then, _ := row["then"].(map[string]any)
				r := exemptionRule{ID: fmt.Sprint(row["id"])}
				if v, ok := when["annual_turnover_below_kobo"].(float64); ok {
					r.AnnualTurnoverBelowKobo = uint64(v)
				}
				if v, ok := when["fixed_assets_below_kobo"].(float64); ok {
					r.FixedAssetsBelowKobo = uint64(v)
				}
				r.IsCompany, _ = when["is_company"].(bool)
				r.Reason = fmt.Sprint(then["reason"])
				e.exemptions = append(e.exemptions, r)
			}
		}
	}
	return e, nil
}

// BandResult is the outcome of a band evaluation.
type BandResult struct {
	Band            string   `json:"band"`
	BandLabel       string   `json:"band_label"`
	AnnualLevyKobo  uint64   `json:"annual_levy_kobo"`
	MonthlyLevyKobo uint64   `json:"monthly_levy_kobo"`
	AdminFeeKobo    uint64   `json:"admin_fee_kobo"`
	Exempt          bool     `json:"exempt"`
	ExemptReason    string   `json:"exempt_reason,omitempty"`
	Graduate        bool     `json:"graduate"`
	PackID          string   `json:"pack_id"`
	PackVersion     string   `json:"pack_version"`
	Trace           []string `json:"trace"`
}

// packForState resolves the state pack with federal fallback.
func (e *BandEngine) packForState(state string) Pack {
	key := "rp-presumptive-" + strings.ToLower(strings.ReplaceAll(strings.TrimSpace(state), " ", "-"))
	if p, ok := e.packs[key]; ok {
		return p
	}
	return e.packs["rp-presumptive-federal"]
}

// Evaluate determines the turnover band and presumptive levy for an operator.
func (e *BandEngine) Evaluate(state, tradeCategory string, annualTurnoverKobo uint64, isCompany bool, fixedAssetsKobo uint64) BandResult {
	res := BandResult{Trace: []string{}}
	// 1) exemptions (rp-exemption-nta)
	for _, ex := range e.exemptions {
		if ex.AnnualTurnoverBelowKobo > 0 && annualTurnoverKobo >= ex.AnnualTurnoverBelowKobo {
			continue
		}
		if ex.IsCompany && !isCompany {
			continue
		}
		if ex.FixedAssetsBelowKobo > 0 && fixedAssetsKobo >= ex.FixedAssetsBelowKobo {
			continue
		}
		res.Exempt = true
		res.ExemptReason = ex.Reason
		res.PackID = "rp-exemption-nta"
		res.PackVersion = e.packs["rp-exemption-nta"].Version
		res.Trace = append(res.Trace, fmt.Sprintf("rp-exemption-nta rule %s matched -> exempt", ex.ID))
		return res
	}
	res.Trace = append(res.Trace, "rp-exemption-nta: no exemption matched")
	// 2) turnover band (rp-turnover-bands)
	var band *TurnoverBand
	for i := range e.bands {
		b := &e.bands[i]
		if annualTurnoverKobo < b.MinKobo {
			continue
		}
		if b.MaxKobo != nil && annualTurnoverKobo > *b.MaxKobo {
			continue
		}
		band = b
		break
	}
	if band == nil {
		res.Trace = append(res.Trace, "rp-turnover-bands: no band matched (treated as above_ceiling)")
		band = &TurnoverBand{ID: "above_ceiling", Label: "Above presumptive ceiling", Graduate: true}
	}
	res.Band = band.ID
	res.BandLabel = band.Label
	res.Trace = append(res.Trace, fmt.Sprintf("rp-turnover-bands: turnover %d kobo -> band %s", annualTurnoverKobo, band.ID))
	if band.Graduate {
		res.Graduate = true
		res.Trace = append(res.Trace, "above presumptive ceiling: route to standard regime (MBS)")
		return res
	}
	// 3) levy (rp-presumptive-<state>|federal)
	p := e.packForState(state)
	res.PackID = p.ID
	res.PackVersion = p.Version
	table, _ := p.Rules["levy_table_annual_kobo"].(map[string]any)
	row, _ := table[band.ID].(map[string]any)
	cat := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(tradeCategory), " ", "_"))
	levyF, ok := row[cat].(float64)
	if !ok {
		levyF, _ = row["default"].(float64)
		res.Trace = append(res.Trace, fmt.Sprintf("%s: category %q not in table; default levy used", p.ID, cat))
	} else {
		res.Trace = append(res.Trace, fmt.Sprintf("%s: band %s x category %s -> levy %d kobo", p.ID, band.ID, cat, uint64(levyF)))
	}
	res.AnnualLevyKobo = uint64(levyF)
	res.MonthlyLevyKobo = res.AnnualLevyKobo / 12
	if fee, ok := p.Rules["admin_fee_kobo"].(float64); ok {
		res.AdminFeeKobo = uint64(fee)
	}
	return res
}

// Packs returns all loaded packs (for the /v1/packs endpoint).
func (e *BandEngine) Packs() []Pack {
	out := make([]Pack, 0, len(e.packs))
	for _, p := range e.packs {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
