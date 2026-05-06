---
name: ringbinder
description: Use ringbinder to find documents, list documents, answer with citations, and propose file renames.
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

3. Parse result fields:
   - `path`, `page_index`, `page_count`, `snippet`, `rank`, `search_source`
   - `search_source` is one of: `fts`, `trigram`, `path`

4. Merge candidates:
   - dedupe by `(path, page_index)`
   - prefer `fts` over `trigram` over `path` when evidence quality conflicts
   - keep ~10–30 pages for reading

5. Read full text before answering:
   - `ringbinder read --json --path <path> --page <i> --context 1`
   - use `--start/--end` for wider ranges when needed

6. Optional metadata for ranking/citations:
   - `ringbinder doc get --json --path <path>`

7. Answer with quotes and citations:
   - quote exact supporting lines
   - cite as `path (page X)` with human 1-based page numbers

### Rules
- Never guess; only claim what you can quote from `read` output.
- If evidence is weak, run more probes (OR/raw/trigram) or ask one targeted clarifying question.
- Prefer reading fewer pages deeply over many snippets shallowly.
- When using `--fts`, prefer known-good patterns; if it errors, simplify.
