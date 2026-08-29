---
name: ringbinder
description: Use ringbinder to find documents, list documents, answer with citations.
---

# Ringbinder

Use `ringbinder help` and `ringbinder help [command]` to learn what's possible.

## List recent documents, or by date / time range

By default `ringbinder doc list` lists 50 recent docs. Read `ringbinder doc list --help` to learn what else is possible.

## Find information in documents

### Retrieval loop
1. Build probe set (5–20 probes):
   - precision probes: `--mode and`
   - recall probes: `--mode or`
   - expert probes: `--fts '<raw fts5 query>'`
   - OCR-noise fallback: repeat key probes with `--trigram`

### Raw FTS safety
- `--fts` is good; prefer known-good patterns: terms, quoted phrases, `AND`/`OR`/`NOT`, and parentheses.
- Don’t guess advanced syntax like `NEAR/5` unless you’ve confirmed ringbinder supports it.
- If a raw FTS query errors, simplify it or split it into multiple `--fts` probes.

2. Run each probe:
   - `ringbinder find --json --limit 50 --offset 0 <query>`
   - or `ringbinder find --json --fts '<raw>'`

3. Parse `find --json` result fields:
   - `path`, `page_index`, `page_count`, `snippet`, `rank`, `search_source`
   - nullable `model` (the exact OCR identifier for text results; `null` for path-only results)
   - `ocr_pending` (whether the document still lacks OCR pages)
   - `search_source` is one of: `fts`, `trigram`, `path`

4. Merge candidates:
   - dedupe by `(path, page_index)`
   - prefer `fts` over `trigram` over `path` when evidence quality conflicts
   - do not try to read results where `ocr_pending` is `true`; path-only matches can identify an incomplete document but provide no OCR evidence
   - keep ~10–30 complete pages for reading

5. Read full text before answering:
   - `ringbinder read --json --path <path> --page <i> --context 1`
   - each returned page has `page_index`, `markdown`, and nullable exact `model`
   - use `--start/--end` for wider ranges when needed

6. Optional metadata for ranking/citations:
   - `ringbinder doc get --json --path <path>`
   - document JSON has `path`, `created_at`, `modified_at`, `page_count`, `ocr_pages_completed`, `models`, `ocr_pending`, and `deleted`
   - `models` contains `{model, page_count}` entries; `model` is the exact identifier for that page and can be `null` for older results

7. Answer with quotes and citations:
   - quote exact supporting lines
   - cite as `path (page X)` with human 1-based page numbers

### Rules
- Never guess; only claim what you can quote from `read` output.
- If evidence is weak, run more probes (OR/raw/trigram) or ask one targeted clarifying question.
- Prefer reading fewer pages deeply over many snippets shallowly.
- When using `--fts`, prefer known-good patterns; if it errors, simplify.
