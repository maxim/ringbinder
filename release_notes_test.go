package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseNotesScript(t *testing.T) {
	changelog := `# Changelog

## [Unreleased]

### Changed

- Future work.

## [0.3.0] - 2026-09-02

### Added

- Added direct-commit support.
- Added pull-request support.

## [0.2.0] - 2026-08-13

- Earlier work.
`
	got, err := runReleaseNotes(t, changelog, "v0.3.0", "v0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	want := `### Added

- Added direct-commit support.
- Added pull-request support.

**Full Changelog**: https://github.com/maxim/ringbinder/compare/v0.2.0...v0.3.0
`
	if got != want {
		t.Fatalf("release notes = %q, want %q", got, want)
	}
}

func TestReleaseNotesScriptOmitsComparisonForFirstRelease(t *testing.T) {
	changelog := `# Changelog

## [0.1.0] - 2026-08-05

### Added

- Initial release.

[Unreleased]: https://github.com/maxim/ringbinder/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/maxim/ringbinder/releases/tag/v0.1.0
`
	got, err := runReleaseNotes(t, changelog, "v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	want := "### Added\n\n- Initial release.\n"
	if got != want {
		t.Fatalf("release notes = %q, want %q", got, want)
	}
}

func TestReleaseNotesScriptRejectsInvalidChangelog(t *testing.T) {
	tests := map[string]struct {
		changelog string
		wantError string
	}{
		"missing version": {
			changelog: "# Changelog\n\n## [0.2.0] - 2026-08-13\n\n- Earlier work.\n",
			wantError: "exactly one ## [0.3.0] - YYYY-MM-DD heading",
		},
		"duplicate version": {
			changelog: "## [0.3.0] - 2026-09-02\n\n- One.\n\n## [0.3.0] - 2026-09-03\n\n- Two.\n",
			wantError: "exactly one ## [0.3.0] - YYYY-MM-DD heading",
		},
		"malformed date": {
			changelog: "## [0.3.0] - September 2, 2026\n\n- Work.\n",
			wantError: "exactly one ## [0.3.0] - YYYY-MM-DD heading",
		},
		"empty section": {
			changelog: "## [0.3.0] - 2026-09-02\n\n### Added\n\n## [0.2.0] - 2026-08-13\n\n- Earlier work.\n",
			wantError: "must contain at least one release-note bullet",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			output, err := runReleaseNotes(t, test.changelog, "v0.3.0", "v0.2.0")
			if err == nil {
				t.Fatalf("release notes succeeded with output %q", output)
			}
			if !strings.Contains(output, test.wantError) {
				t.Fatalf("error output = %q, want %q", output, test.wantError)
			}
		})
	}
}

func runReleaseNotes(t *testing.T, changelog string, versions ...string) (string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "CHANGELOG.md")
	if err := os.WriteFile(path, []byte(changelog), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("./scripts/release-notes", versions...)
	command.Env = append(os.Environ(), "CHANGELOG_FILE="+path)
	output, err := command.CombinedOutput()
	return string(output), err
}
