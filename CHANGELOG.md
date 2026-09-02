# Changelog

Notable changes to Ringbinder are documented here.

## [Unreleased]

### Changed

- GitHub Releases now use the matching versioned changelog section, so release notes include direct commits as well as merged pull requests.

## [0.3.0] - 2026-09-02

### Added

- Gemini Flash 3.7 is now supported for OCR and is recommended for the best results. Allow fallback to Mistral when Gemini refuses to process a page.
- With Gemini also comes batch support. Gemini Batch API offers half-price processing for documents that can finish later. Ringbinder can submit jobs, resume them after an interruption, keep pages as they finish, retry only failed pages, and clean up uploaded files when the work is done.
- `ringbinder cost` now shows a low-to-high price range based on the services you selected. `ringbinder batch cost` separately estimates Gemini's half-price batch processing. Both count only pages that still need OCR, and `--limit N` lets you preview a smaller set of documents.
- Add support for `exclude` in the config file, to be able to exclude files from sweep. Any excludes added as command arguments are merged with the ones in the file.
- Document, read, and search JSON now shows whether OCR is finished and which service handled each page.

### Changed

- The `--model` argument can now be repeated to set OCR fallback.
- OCR concurrency settings and flags were removed because Ringbinder now chooses safe limits automatically.
- Documents with unfinished OCR stay out of text search and `read` until every page is ready. You can still find them by filename and see that OCR is pending.
- `--redo` was completely removed. To rebuild OCR use a separate database so your current searchable database remains available until the replacement is ready.

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

[Unreleased]: https://github.com/maxim/ringbinder/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/maxim/ringbinder/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/maxim/ringbinder/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/maxim/ringbinder/releases/tag/v0.1.0
