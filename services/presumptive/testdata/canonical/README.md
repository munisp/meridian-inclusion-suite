Canonical snapshot of the signed rule packs, transcribed from
meridian-rule-packs @ f5543879:
  packs/rp-turnover-bands/1.0.0.yaml
  packs/rp-presumptive-federal/1.0.0.yaml (table + presumptive.exempt.micro)
  packs/rp-presumptive-lagos/1.0.0.yaml
  packs/rp-presumptive-kano/1.0.0.yaml
  packs/rp-exemption-nta/1.0.0.yaml (exempt.small-company thresholds)
b3_pack_drift_test.go fails if the embedded packs/*.json diverge from this
canonical snapshot. When the signed packs are regazetted, update BOTH.
