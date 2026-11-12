# Releasing zxporter

zxporter is developed and released from DevZero's internal monorepo, where
this directory lives at `operators/zxporter/`. The public
[devzero-inc/zxporter](https://github.com/devzero-inc/zxporter) repository is
a published mirror: each release pushes this directory's tree to it as branch
`release/vX.Y.Z`, tags that commit `vX.Y.Z`, and opens a PR to bring its `main`
up to date.

## Consuming a release

Each release `vX.Y.Z` publishes:

- images `zxporter`, `zxporter-nodemon` and `zxporter-netmon`, tag `vX.Y.Z`, on
  `docker.io/devzeroinc/` and `public.ecr.aws/devzeroinc/`
- Helm charts `zxporter` (bundles `zxporter-nodemon`), `zxporter-nodemon` and
  `zxporter-netmon`, version `X.Y.Z`, at `oci://registry-1.docker.io/devzeroinc`
- the `vX.Y.Z` tag on the public mirror, whose `helm-chart/` and `dist/` carry
  that version (pin it as an ArgoCD `targetRevision`)

## Cutting a release

From the monorepo root: `just release zxporter [vX.Y.Z]`. The full procedure,
what the release workflow does, how the version reaches DevZero's backend, and
recovery and rollback are in the root `RELEASE.md` of the monorepo (section
"zxporter").

`.github/workflows/release.yml` and the `release` recipe in `justfile` in this
directory are the pre-monorepo release flow. They are still present in the
mirror but are not how zxporter is released; do not run them.
