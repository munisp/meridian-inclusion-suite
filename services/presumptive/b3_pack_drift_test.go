package main

import (
	"encoding/json"
	"os"
	"testing"
)

// B3 #3 drift guard: the embedded packs/*.json must stay in lockstep with
// the canonical signed rule packs (snapshot under testdata/canonical,
// transcribed from meridian-rule-packs @ f5543879). Any divergence fails.
// Also B3 #19: exemption and band boundary semantics regression.

func loadJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func TestB3TurnoverBandsMatchCanonical(t *testing.T) {
	var canon struct {
		BoundarySemantics string `json:"boundary_semantics"`
		Bands             []struct {
			ID      string `json:"id"`
			MinKobo uint64 `json:"min_kobo"`
			MaxKobo uint64 `json:"max_kobo"`
		} `json:"bands"`
		ExitGteKobo uint64 `json:"exit_gte_kobo"`
	}
	loadJSON(t, "testdata/canonical/rp-turnover-bands.canonical.json", &canon)

	e, err := LoadBandEngine()
	if err != nil {
		t.Fatal(err)
	}
	if len(e.bands) != len(canon.Bands) {
		t.Fatalf("embedded %d bands vs canonical %d", len(e.bands), len(canon.Bands))
	}
	for i, cb := range canon.Bands {
		eb := e.bands[i]
		if eb.ID != cb.ID || eb.MinKobo != cb.MinKobo {
			t.Errorf("band %d: embedded %s min %d vs canonical %s min %d", i, eb.ID, eb.MinKobo, cb.ID, cb.MinKobo)
		}
		if cb.MaxKobo > 0 {
			if eb.MaxKobo == nil || *eb.MaxKobo != cb.MaxKobo {
				t.Errorf("band %s: embedded max %v vs canonical %d", cb.ID, eb.MaxKobo, cb.MaxKobo)
			}
		}
		// graduation is handled by the no-band-match fallback; embedded
		// bands must not carry a graduate row below the exit threshold
		if eb.Graduate && eb.MinKobo < canon.ExitGteKobo {
			t.Errorf("band %s graduates below the canonical ceiling", eb.ID)
		}
	}
}

func TestB3LevyTablesMatchCanonical(t *testing.T) {
	var canon map[string]map[string]uint64
	loadJSON(t, "testdata/canonical/levy-tables.canonical.json", &canon)
	e, err := LoadBandEngine()
	if err != nil {
		t.Fatal(err)
	}
	for packID, table := range canon {
		p, ok := e.packs[packID]
		if !ok {
			t.Errorf("embedded pack %s missing", packID)
			continue
		}
		raw, _ := json.Marshal(p.Rules["levy_table_annual_kobo"])
		var emb map[string]map[string]uint64
		if err := json.Unmarshal(raw, &emb); err != nil {
			t.Errorf("%s levy table parse: %v", packID, err)
			continue
		}
		for band, want := range table {
			row, ok := emb[band]
			if !ok {
				t.Errorf("%s: canonical band %s missing from embedded table", packID, band)
				continue
			}
			if row["default"] != want {
				t.Errorf("%s %s: embedded default %d vs canonical %d", packID, band, row["default"], want)
			}
			if len(row) != 1 {
				t.Errorf("%s %s: embedded table carries non-canonical trade-category overrides %v", packID, band, row)
			}
		}
		for band := range emb {
			if _, ok := table[band]; !ok {
				t.Errorf("%s: embedded band %s not in canonical table", packID, band)
			}
		}
	}
}

func TestB3ExemptionsMatchCanonicalAndBoundarySemantics(t *testing.T) {
	var canon struct {
		PitLte        uint64 `json:"pit_tax_free_threshold_turnover_lte_kobo"`
		SmallTurnLte  uint64 `json:"small_company_turnover_lte_kobo"`
		SmallAssetLte uint64 `json:"small_company_fixed_assets_lte_kobo"`
	}
	loadJSON(t, "testdata/canonical/exemptions.canonical.json", &canon)
	e, err := LoadBandEngine()
	if err != nil {
		t.Fatal(err)
	}
	var pit, small *exemptionRule
	for i := range e.exemptions {
		switch e.exemptions[i].ID {
		case "pit_tax_free_threshold":
			pit = &e.exemptions[i]
		case "small_company_relief":
			small = &e.exemptions[i]
		}
	}
	if pit == nil || small == nil {
		t.Fatal("embedded exemptions missing canonical rules")
	}
	if pit.AnnualTurnoverAtOrBelowKobo != canon.PitLte {
		t.Errorf("pit threshold %d vs canonical %d", pit.AnnualTurnoverAtOrBelowKobo, canon.PitLte)
	}
	if small.AnnualTurnoverAtOrBelowKobo != canon.SmallTurnLte || small.FixedAssetsAtOrBelowKobo != canon.SmallAssetLte {
		t.Errorf("small company thresholds %d/%d vs canonical %d/%d",
			small.AnnualTurnoverAtOrBelowKobo, small.FixedAssetsAtOrBelowKobo, canon.SmallTurnLte, canon.SmallAssetLte)
	}

	// B3 #19: exactly AT the exempt line pays nothing (lte semantics)
	if r := e.Evaluate("Lagos", "retail", 80000000, false, 0); !r.Exempt {
		t.Errorf("turnover exactly N800,000.00 must be exempt, got %+v", r)
	}
	if r := e.Evaluate("Lagos", "retail", 80000001, false, 0); r.Exempt {
		t.Errorf("turnover N800,000.01 must NOT be exempt")
	}
	// exactly N100m company with assets below the line: exempt (lte)
	if r := e.Evaluate("Lagos", "retail", 10000000000, true, 1000000); !r.Exempt {
		t.Errorf("company at exactly N100m turnover must be exempt, got %+v", r)
	}

	// B3 #3: [min, max) band semantics — exactly N1m belongs to the HIGHER band
	if r := e.Evaluate("Lagos", "retail", 100000000, false, 0); r.Band != "small" {
		t.Errorf("turnover exactly N1m must be band small (max exclusive), got %s", r.Band)
	}
	if r := e.Evaluate("Lagos", "retail", 99999999, false, 0); r.Band != "micro" {
		t.Errorf("turnover N999,999.99 must be band micro, got %s", r.Band)
	}
	// exactly N100m turnover graduates (ceiling aligned with VAT threshold)
	if r := e.Evaluate("Lagos", "retail", 10000000000, false, 25000000001); !r.Graduate {
		t.Errorf("turnover exactly N100m (non-exempt assets) must graduate, got %+v", r)
	}
	// canonical hand-check: N3m-N5m operator in Lagos = lower_medium N20,000
	if r := e.Evaluate("Lagos", "retail", 400000000, false, 0); r.Band != "lower_medium" || r.AnnualLevyKobo != 2000000 {
		t.Errorf("N4m Lagos: want lower_medium/2,000,000 kobo, got %s/%d", r.Band, r.AnnualLevyKobo)
	}
	// N25m-N100m operators are NOT graduated (old pack wrongly exited at N25m)
	if r := e.Evaluate("Lagos", "retail", 5000000000, false, 0); r.Graduate {
		t.Errorf("N50m operator must not graduate, got %+v", r)
	}
}
