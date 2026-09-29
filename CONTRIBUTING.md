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
5. Merge after validation. CI publishes `X.Y.Z` and updates the `latest` image
   tag. Confirm both release artifacts and the workflow result before upgrading
   consumers. Publishing does not establish live deployment qualification.
