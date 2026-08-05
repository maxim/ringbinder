# Releasing Ringbinder

Ringbinder publishes stable releases through the manual **Release** GitHub Actions workflow. GoReleaser builds four standalone archives, publishes the GitHub Release, and opens a formula pull request in [`maxim/homebrew-tap`](https://github.com/maxim/homebrew-tap). Do not create production tags or releases locally.

## One-time setup

1. Create a fine-grained GitHub personal access token owned by `maxim` and limited to the `maxim/homebrew-tap` repository.
2. Grant only **Contents: Read and write** and **Pull requests: Read and write** repository permissions. The token must be able to push the formula branch, open its pull request, and enable auto-merge.
3. Store it in the Ringbinder repository as the Actions secret `HOMEBREW_TAP_TOKEN`. Paste the token into this command's standard input when prompted:

   ```sh
   gh secret set HOMEBREW_TAP_TOKEN --repo maxim/ringbinder
   ```

4. In the tap repository, enable auto-merge and require both `brew test-bot` matrix checks on `main`: macOS and Linux. Do not allow either check to be bypassed for formula pull requests.

The tap intentionally retains only its generated test workflow. GoReleaser owns formula updates, so Homebrew autobump and bottle-publishing workflows are disabled.

## Optional local validation

Use GoReleaser `v2.15.4`, matching the production workflow. This is the last release whose formula generator passes `goreleaser check`; newer GoReleaser versions require casks, while Ringbinder intentionally uses an architecture-selecting formula. Install that tool version only on the maintainer machine, then run:

```sh
go install github.com/goreleaser/goreleaser/v2@v2.15.4
goreleaser check
goreleaser release --snapshot --clean
```

The snapshot must contain exactly four `.tar.gz` archives plus a four-entry `checksums.txt`. Each archive must contain only `ringbinder` and `LICENSE`, all checksums must verify, and the generated formula must pass Ruby syntax validation and reference all four archives with matching checksums. Run the native binary from the host-matching archive and confirm that `--version` prints the exact snapshot version and an isolated database smoke test succeeds:

```sh
snapshot_version="$(ruby -rjson -e 'print JSON.parse(File.read("dist/metadata.json")).fetch("version")')"
goos="$(go env GOOS)"
goarch="$(go env GOARCH)"
archive="dist/ringbinder_${snapshot_version}_${goos}_${goarch}.tar.gz"
test -f "$archive"

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT
mkdir "$tmpdir/archive" "$tmpdir/home"
tar -xzf "$archive" -C "$tmpdir/archive"
test "$(HOME="$tmpdir/home" "$tmpdir/archive/ringbinder" --version)" = \
  "ringbinder version v${snapshot_version}"
test "$(HOME="$tmpdir/home" "$tmpdir/archive/ringbinder" \
  --database "$tmpdir/ringbinder.db" cost)" = "No documents pending OCR."
test -f "$tmpdir/ringbinder.db"
rm -rf "$tmpdir"
trap - EXIT
```

GoReleaser is release tooling only. It is not a Ringbinder or Homebrew runtime dependency.

## Production release

1. Ensure `main` is clean and contains every intended release change.
2. Run `go test ./...` locally.
3. Open [Ringbinder Actions](https://github.com/maxim/ringbinder/actions/workflows/release.yml), select **Run workflow**, choose the `main` branch, and enter the next stable tag such as `v0.1.0`. The first stable tag must be `v0.1.0`; every later input must be strictly greater than the latest stable tag.
4. The serialized workflow captures the selected `main` SHA, rejects stale or existing versions, and preflights the dedicated tap token's Contents and pull-request write access without creating a branch or pull request before any tag is created.
5. Wait for both the macOS and Linux test jobs. They test the exact captured SHA. The release job then runs GoReleaser `v2.15.4` configuration and snapshot validation on that same SHA, including all four archives, checksums, payload identities, exact formula target mappings, and the native Linux amd64 version/database smoke tests.
6. Immediately before tagging, the workflow confirms that the prior latest stable tag has not changed and that the requested tag still does not exist. It then tags the tested SHA, publishes the release, and opens the tap formula pull request.
7. The workflow registers that exact pull request for squash auto-merge and remains running until the required Homebrew checks merge it. A closed pull request or merge timeout fails the workflow.
8. After the workflow succeeds, confirm the release and installation, substituting the released version where shown:

   ```sh
   version=v0.1.0
   gh release view "$version" --repo maxim/ringbinder
   brew uninstall ringbinder 2>/dev/null || true
   brew untap maxim/tap 2>/dev/null || true
   brew install maxim/tap/ringbinder
   prefix="$(brew --prefix maxim/tap/ringbinder)"
   test -x "$prefix/bin/ringbinder"
   test "$("$prefix/bin/ringbinder" --version)" = "ringbinder version $version"
   "$prefix/bin/ringbinder" --help >/dev/null
   tmpdir="$(mktemp -d)"
   "$prefix/bin/ringbinder" --database "$tmpdir/ringbinder.db" cost
   test -f "$tmpdir/ringbinder.db"
   test -z "$(brew deps maxim/tap/ringbinder)"
   ```

The cost command must print `No documents pending OCR.` The empty dependency check confirms that the prebuilt formula has no runtime dependencies, including Go.

## Failure recovery

Published tags are immutable. Never delete, recreate, or move a tag that has been pushed, even if archive publishing or the tap update fails. Fix the release problem on `main` and dispatch the next patch version.

If testing fails before tagging, fix `main` and rerun the same requested version. If the workflow pushes the tag but any later publishing step fails, use a new patch version. If the GitHub Release succeeds but the tap pull request fails its checks, leave the released tag and artifacts intact, fix the formula/release configuration on `main`, and publish a new patch release.

The initial macOS archives are intentionally unsigned and unnotarized. Homebrew is the supported prebuilt installation path and verifies each immutable archive with its pinned SHA-256 checksum. Signing or notarization can be added later if direct browser downloads become a supported installation method.
