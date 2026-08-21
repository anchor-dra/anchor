# Release process

Anchor uses conventional commits and one GitHub Actions workflow for validation
and releases. Pull requests run validation only. A push to `develop` publishes a
release candidate; merging that exact tested tree to `main` promotes it to the
matching stable version.

The first `0.2` `develop` release is bootstrapped as `v0.2.0-rc.1` from the matching
`0.2.0` versions in `release.yaml` and `charts/anchor/Chart.yaml`. Later
candidate versions are calculated from commits since the latest stable tag:

| Commit | Release |
|---|---|
| `feat!: ...` or a `BREAKING CHANGE:` footer | major |
| `feat: ...` | minor |
| `fix: ...` or `perf: ...` | patch |
| `docs:`, `test:`, `chore:`, `ci:` and other types | no release |

Scopes are supported, for example `feat(controller): ...`. The highest change
in a push wins. Additional pushes for the same base version increment the RC
number (`rc.1`, `rc.2`, and so on).

`main` does not calculate a different version. It finds the newest RC whose Git
tree exactly matches `main` and promotes `vX.Y.Z-rc.N` to `vX.Y.Z`. This prevents
untested content from being published as stable. Merge `develop` into `main`
without adding changes during the promotion; both merge commits and squash
merges are accepted when the resulting trees are identical.

## Published artifacts

For candidate `0.2.0-rc.1`, the `develop` workflow creates:

- GitHub prerelease and tag `v0.2.0-rc.1`;
- image `ghcr.io/anchor-dra/anchor:0.2.0-rc.1` and `rc-latest`;
- Helm chart `oci://ghcr.io/anchor-dra/charts/anchor` version `0.2.0-rc.1`;
- packaged chart, versioned `release.yaml`, and `SHA256SUMS` release assets.

After promotion, the `main` workflow creates:

- GitHub release and tag `v0.2.0`;
- Linux AMD64 image `ghcr.io/anchor-dra/anchor:0.2.0`, plus immutable commit and
  convenience `latest` tags;
- Helm chart `oci://ghcr.io/anchor-dra/charts/anchor` version `0.2.0`;
- packaged chart, versioned `release.yaml`, and `SHA256SUMS` release assets.

The Go binary version, container tag, chart version, chart `appVersion`, and
release asset version are all set from the version calculated by the workflow.
No release commit is pushed back to `main`; Git tags and published artifacts are
the release-version source of truth.

## Repository setup

The repository must live at `github.com/anchor-dra/anchor`. In **Settings →
Actions → General**, allow GitHub Actions read and write access. The workflow's
`GITHUB_TOKEN` needs only `contents: write` and `packages: write`, both declared
on the release job; no personal token or registry password is required.

After the first run, make the `anchor` image package and the `charts/anchor`
package public in the GitHub organization so connected installations do not
need registry credentials.

Protect both `develop` and `main` with pull requests and require the `Validate`
job. Feature and fix pull requests target `develop`. Stable promotion is a
`develop` to `main` merge after the desired RC has passed its deployment tests.

## Maintainer verification

Before merging a release-bearing change, run:

```bash
./scripts/check.sh
make image VERSION=ci
```

After the `develop` workflow completes, verify the RC packages and assets:

```bash
docker pull ghcr.io/anchor-dra/anchor:0.2.0-rc.1
helm pull oci://ghcr.io/anchor-dra/charts/anchor --version 0.2.0-rc.1
gh release view v0.2.0-rc.1 --repo anchor-dra/anchor
```

After promotion to `main`, verify the stable packages and release:

```bash
docker pull ghcr.io/anchor-dra/anchor:0.2.0
helm pull oci://ghcr.io/anchor-dra/charts/anchor --version 0.2.0
gh release view v0.2.0 --repo anchor-dra/anchor
```

Do not create RC or stable tags by hand. Manually created tags bypass the
workflow's version decision and do not publish the image or chart.
