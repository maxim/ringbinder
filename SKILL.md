---
name: ringbinder
description: Use ringbinder CLI to retrieve OCR evidence from SQLite FTS, answer with citations, and propose safe file renames (confirm before applying).
compatibility: Requires ringbinder in PATH and a populated DB (run ringbinder sweep + ringbinder ocr first).
---

# Ringbinder

## Setup
- Populate/update index:
  - `ringbinder sweep <paths...>`
  - `ringbinder ocr`
- Prefer machine output: `--json` (find uses NDJSON).

## Capability A: Evidence-based Q&A (no embeddings)

### Retrieval loop
1. Build probe set (5–20 probes):
   - precision probes: `--mode and`
   - recall probes: `--mode or`
   - expert probes: `--fts '<raw fts5 query>'`
   - OCR-noise fallback: repeat key probes with `--trigram`

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

## Capability B: Propose and apply renames for timestamp-like filenames

### Procedure
1. Identify candidates with timestamp-like basenames.
2. Read OCR for each candidate:
   - start: `ringbinder read --json --path <path> --page 0 --context 1`
   - expand pages only if needed.
3. Extract date:
   - prefer semantic doc date from OCR
   - fallback to filename date only if OCR date is unreliable
   - format as `YYYY-MM-DD`, or `YYYY-MM`, or `YYYY`.
4. Create title from OCR (short, specific, filesystem-safe).
5. Propose filename: `<date> - <title>.<ext>`.
6. Present full rename plan (`OLD -> NEW`) and ask for explicit confirmation.
7. After confirmation, rename files and refresh Ringbinder paths (rename command if available, otherwise `ringbinder sweep` on affected roots).

### Rules
- Do not rename anything before explicit user confirmation.
- Every proposed name must include a normalized date.
- Do not invent titles; base them on retrieved OCR text.
