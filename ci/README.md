# ci/

`workflows/ci.yml` is the CI workflow, kept here per HARDENING.md H6: the
GitHub token used for this push lacks the `workflow` scope, so pushing
`.github/workflows/ci.yml` was rejected. **Manual step**: move/copy this
file to `.github/workflows/ci.yml` with a workflow-scoped token. It runs:
Go `build`/`vet`/`test -race` (whole module), education `pytest`, and
`npm ci` + `npm run build` for both PWAs.
