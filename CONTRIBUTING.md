# Contributing

Run `scripts/check.sh` before submitting changes. Use semantic commit messages,
keep AWS mutations behind `PlacementStrategy`, and add tests for every safety
or conflict rule. Developer certificate-of-origin sign-off will be introduced
before the first public release.

## Release flow

1. Branch from `develop`, run the checks, and open a pull request to `develop`.
   Keep `release.yaml` metadata version, chart version/appVersion and the Makefile
   default aligned with the intended stable version.
2. Use conventional commits: `fix:`/`perf:` selects a patch release, `feat:` a
   minor release, and a breaking-change marker a major release. CI calculates the
   version from commits since the latest stable tag; metadata does not override it.
3. Merge the validated pull request into `develop`. CI validates again and
   publishes the image, chart and GitHub prerelease as `X.Y.Z-rc.N`.
4. After successful RC publication, open a `develop` to `main` pull request.
   Preserve the RC's complete file tree; the stable release gate rejects any
   different content. Do not make a separate version-bump commit on `main`.
5. Main PRs and pushes verify identical RC content, a successful develop workflow,
   uploaded release files and available image/chart manifests. They reuse that
   validation instead of repeating the full suite. CI publishes `X.Y.Z` and updates the `latest` image
   tag. Confirm both release artifacts and the workflow result before upgrading
   consumers. Publishing does not establish live deployment qualification.

## CI caches and builds

- Go checks use the standard `go env GOCACHE`, or an explicitly supplied
  `GOCACHE`, so `setup-go` restores the cache actually used by tests and vet.
- Terraform provider downloads are cached by OS, architecture, Terraform version
  and the committed lockfile. Initialization uses `-lockfile=readonly`.
  When updating providers, record checksums for both CI and local development:
  `terraform -chdir=hack/e2e/aws providers lock -platform=linux_amd64 -platform=darwin_arm64`.
  Linux's unpacked-package checksum must be committed; archive checksums alone
  are insufficient for subsequent validation with read-only initialization.
- Pull requests to develop validate the image without pushing it. Develop/main
  publish one image per release; the validation job does not build a second one.
- Docker uses the GHCR `buildcache` tag with intermediate layers (`mode=max`),
  shared across branches. Only trusted develop/main release jobs write this cache;
  PRs only read it. A cache miss falls back to a normal build. The first release
  with this workflow seeds the cache. Go package compilation is cached before
  applying the version linker flag, so stable images report the stable version.
- Main promotion requires public access to the published RC image/chart manifests,
  as used by Anchor consumers. It fails closed if the release evidence is missing.

Local pre-commit hooks are not required. `scripts/check.sh` remains the complete
local validation entry point, including promotion-gate tests.
