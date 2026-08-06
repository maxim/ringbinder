# Releasing Ringbinder

Ringbinder publishes stable releases through the manual **Release** GitHub Actions workflow. GitHub Releases contain release notes and GitHub's automatic source links only. Homebrew compiles the tagged source and is the supported installer on macOS and Linux.

Do not create production tags, GitHub Releases, or tap update branches manually. The workflow pins every release to the exact commit tested on both platforms.

## One-time setup

1. Create a fine-grained GitHub personal access token owned by `maxim` and limited to [`maxim/homebrew-tap`](https://github.com/maxim/homebrew-tap).
2. Grant only **Contents: Read and write** and **Pull requests: Read and write**. The token must be able to push the bump branch, open its pull request, and squash-merge it after required checks pass.
3. Store it in Ringbinder as the Actions secret `HOMEBREW_TAP_TOKEN`:

   ```sh
   gh secret set HOMEBREW_TAP_TOKEN --repo maxim/ringbinder
   ```

4. Protect the tap's `main` branch with strict, squash-only pull requests and exactly these required checks from the GitHub Actions integration (`integration_id: 15368`), with no bypass actors:

   - `test-bot (macos-26)`
   - `test-bot (ubuntu-latest, ghcr.io/homebrew/brew:main, --privileged)`

5. Protect `refs/tags/v*` in Ringbinder with update and deletion rules. Do not add a creation rule because the release workflow creates new version tags.
6. Enable immutable releases in Ringbinder. This setting applies only to releases created after it is enabled; the tag ruleset separately protects older version tags.

The expected check names are a release contract. Changing a runner, container, matrix label, or job name requires one coordinated change to:

- [`maxim/homebrew-tap/.github/workflows/tests.yml`](https://github.com/maxim/homebrew-tap/blob/main/.github/workflows/tests.yml)
- the tap `main` ruleset's required contexts
- Ringbinder's expected-context list in [`.github/workflows/release.yml`](.github/workflows/release.yml)

A partial change intentionally stops release automation rather than accepting a missing or stale check.

## Optional local validation

Run the tests and the same kind of host build used by the formula, substituting the intended version:

```sh
set -euo pipefail
version=v0.2.0
go test ./...

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT
mkdir "$tmpdir/home"
CGO_ENABLED=0 go build \
  -trimpath \
  -ldflags="-s -w -X github.com/maxim/ringbinder/cmd.version=$version" \
  -o "$tmpdir/ringbinder" \
  .
test "$(HOME="$tmpdir/home" "$tmpdir/ringbinder" --version)" = \
  "ringbinder version $version"
HOME="$tmpdir/home" "$tmpdir/ringbinder" --help >/dev/null
test "$(HOME="$tmpdir/home" "$tmpdir/ringbinder" \
  --database "$tmpdir/ringbinder.db" cost)" = "No documents pending OCR."
test -f "$tmpdir/ringbinder.db"
```

Go is a formula build dependency, not an installed Ringbinder dependency. Homebrew may download normal Go modules while compiling the pinned source.

Formula changes can be checked from Homebrew's active tap checkout with:

```sh
tap_repo="$(brew --repo maxim/tap)"
cd "$tap_repo"
test "$PWD" = "$tap_repo"
ruby -c Formula/ringbinder.rb
brew style Formula/ringbinder.rb
HOMEBREW_NO_INSTALL_FROM_API=1 brew audit --strict maxim/tap/ringbinder
```

## Production release

1. Ensure `main` is clean and contains every intended release change.
2. Run `go test ./...` locally.
3. Open [Ringbinder Actions](https://github.com/maxim/ringbinder/actions/workflows/release.yml), select **Run workflow**, choose `main`, and enter the next stable tag such as `v0.2.0`.
4. Wait for the release workflow and the tap's macOS/Linux jobs to finish.

The workflow:

1. Captures the selected `main` SHA, validates stable SemVer ordering, and preflights the dedicated tap token.
2. Runs `go test ./...` plus an exact-version source-build smoke test on macOS and Linux.
3. Rechecks the remote tag state, creates a protected tag when needed, and creates an immutable, zero-upload GitHub Release with `gh release create --verify-tag --generate-notes`.
4. Uses a pinned Homebrew version and `brew bump-formula-pr --tag … --revision … --no-fork --no-browse`. Pinning keeps generated formula bytes reproducible across interrupted runs. Homebrew updates the source tag and commit and removes an old formula `revision` when the upstream version changes.
5. Derives the expected formula bytes with the same Homebrew tooling, then accepts only the exact expected tap repository, branch, pull request, file, blob, and head commit.
6. Updates an otherwise exact stale pull request from tap `main`, waits for fresh copies of both exact required checks, and squash-merges only the pinned head with `--match-head-commit`.
7. Confirms that the expected formula blob reached tap `main` and removes authenticated tap configuration from the runner.

## Verification

Confirm the tag, release, formula, and installation after the workflow succeeds:

```sh
set -euo pipefail
version=v0.2.0
git fetch origin "refs/tags/$version:refs/tags/$version"
sha="$(gh api "repos/maxim/ringbinder/git/ref/tags/$version" --jq .object.sha)"
test "$sha" = "$(git rev-list -n1 "$version")"
test "$(gh api "repos/maxim/ringbinder/releases/tags/$version" --jq .tag_name)" = "$version"
test "$(gh api "repos/maxim/ringbinder/releases/tags/$version" --jq '.assets | length')" = 0
test "$(gh api "repos/maxim/ringbinder/releases/tags/$version" --jq .immutable)" = true

brew update
brew install maxim/tap/ringbinder 2>/dev/null || brew upgrade ringbinder
prefix="$(brew --prefix maxim/tap/ringbinder)"
test "$("$prefix/bin/ringbinder" --version)" = "ringbinder version $version"
"$prefix/bin/ringbinder" --help >/dev/null

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT
test "$("$prefix/bin/ringbinder" --database "$tmpdir/ringbinder.db" cost)" = \
  "No documents pending OCR."
test -f "$tmpdir/ringbinder.db"
test -z "$(brew deps maxim/tap/ringbinder)"
brew deps --include-build maxim/tap/ringbinder | grep -Fx go
```

The empty default dependency output confirms there are no runtime formula dependencies. The build-inclusive output confirms Go is used only while Homebrew compiles Ringbinder.

For the one-time `v0.1.0` formula transition, an installation made with the old formula becomes `0.1.0_1`. If Homebrew still has stale formula metadata, run `brew update` first, verify `brew outdated ringbinder` reports it, and then use the normal `brew upgrade ringbinder`. Ringbinder's config and database are outside Homebrew's prefix and remain untouched.

## Failure recovery

Before the tag is created, fix `main` and rerun the same requested version.

After tagging, rerun only when recovering the latest stable version at the exact SHA captured by the failed run. The workflow can safely converge these interrupted states:

- the exact tag exists but the GitHub Release does not;
- the exact immutable, zero-upload release exists but the bump branch does not;
- the exact bump branch exists but its pull request does not;
- the exact open pull request exists but checks or merging did not finish.

Every recovered tag, release, branch, pull request, head, changed file, and formula blob must match exactly. Any divergence fails closed. Never move a version tag, force-push a bump branch, recreate a pull request over different bytes, or rerun Homebrew over a duplicate pull request.

An interrupted but byte-for-byte exact publication is resumable. A semantically bad published release is not: immutable releases cannot be deleted and republished at the same version. Fix the problem on `main` and supersede it with a higher patch version.
