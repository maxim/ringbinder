# Changelog

Notable changes to Ringbinder are documented here.

## [Unreleased]

### Added

- Opt-in Gemini OCR with structured searchable page and visual descriptions, provider-specific concurrency, retries, PDF chunking, and actual usage-based run cost reporting.
- Provider selection through `--model mistral|gemini` or config, plus offline provider-specific cost estimates.
- Bounded `cost --limit N` estimates and matching `ocr --limit N` batches for reviewing spend incrementally.
- Explicit discounted Gemini batch OCR under `ringbinder batch`, with durable restart recovery, status/cancel/forget commands, visible blocked-work summaries, partial-range staging and direct-only blocked-range retry, frozen usage billing, remote cleanup, machine-readable status/failures, and fail-fast per-database OCR coordination.
- Configurable sweep concurrency through `sweep_concurrency`; `sweep --concurrency` still takes precedence.

### Changed

- `sweep` now reads config when paths and `--database` are explicit unless `--concurrency` is also explicit, allowing `sweep_concurrency` to override the default.
- Removed `--redo` from `sweep`, `cost`, and `ocr`; full OCR rebuilds now use a separate database so the active index remains available for rollback.

## [0.2.0] - 2026-08-13

### Changed

- New OCR requests use Mistral OCR 4.1. Existing OCR results remain unchanged; use `ocr --redo` to explicitly reprocess them.
- Homebrew installs build the tagged Ringbinder source instead of downloading project-built binary archives. Go remains a build-only dependency.

## [0.1.0] - 2026-08-05

### Added

- Initial public release.
- Incremental PDF and image scanning with checksum deduplication, soft deletion, restoration, glob paths, and exclusions.
- Mistral OCR with per-page Markdown, searchable image descriptions, cost estimates, retries, concurrency, and oversized-PDF chunking.
- SQLite FTS5, trigram, and path search, plus page/context reading and JSON output for scripts and agents.
- Homebrew installation on macOS and Linux.

[Unreleased]: https://github.com/maxim/ringbinder/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/maxim/ringbinder/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/maxim/ringbinder/releases/tag/v0.1.0
