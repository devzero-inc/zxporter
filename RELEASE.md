# Releasing zxporter

Releases are **one command**; CI does the rest.

```bash
just tag zxporter vX.Y.Z        # cut release vX.Y.Z
just tag zxporter vX.Y.Z dry    # preview only
just tag-list                   # recent release/chart tags
```

Prereq: `gh` CLI authenticated with access to `devzero-inc/zxporter`.

`just tag` validates `vX.Y.Z` and dispatches the **Release** workflow
(`.github/workflows/release.yml`), which:

1. Bumps chart/image versions, regenerates `Chart.lock` + `dist/`, and tags
   `vX.Y.Z` on the bumped commit.
2. Builds & pushes the `zxporter`, `zxporter-nodemon`, `zxporter-netmon` images
   and publishes the three OCI Helm charts at `vX.Y.Z`.
3. Creates the GitHub Release and triggers the `devzero-inc/services` metadata
   update.

Watch: <https://github.com/devzero-inc/zxporter/actions/workflows/release.yml>

## Merge the two PRs

The release opens two PRs — **approve and merge both** to finish:

1. **zxporter** – `release/bump-vX.Y.Z` (version bump).
2. **../services** – metadata update opened by the metadata retriever workflow.
