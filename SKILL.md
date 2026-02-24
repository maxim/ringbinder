---
name: ringbinder
description: Use the ringbinder CLI to search OCR’d PDFs/images via SQLite FTS, read full OCR pages to answer questions with quoted citations, and propose safe renames for timestamp-named files based on OCR content (ask for confirmation before applying).
compatibility: Requires the ringbinder CLI in PATH and a populated ringbinder SQLite database (run ringbinder sweep + ringbinder ocr first).
---

# Ringbinder

## Setup
- Ensure the database is populated:
  - `ringbinder sweep <paths...>`
  - `ringbinder ocr`
- Use JSON output for automation when available.

## Capability A: Evidence-based Q&A (no embeddings)

### Procedure
1. Create **multiple search probes** (5–20):
   - one precise probe (must-have terms)
   - several broader probes (synonyms, alternate phrasing)
   - include doc-type terms when helpful (invoice/receipt/statement/denial/etc.)
   - if needed, a fallback probe using OR semantics or raw FTS syntax

2. For each probe:
   - run `ringbinder find --json --limit 50 <probe>`
   - union results and dedupe by `(path, page_index)`

3. Select candidates:
   - prefer lower `rank` (bm25) and higher snippet relevance
   - keep ~10–30 candidate pages total

4. Read source pages (verify before answering):
   - `ringbinder read --json --path <path> --page <page_index> --context 1`
   - treat the returned markdown as ground truth

5. Answer with citations:
   - quote the exact lines supporting each factual claim
   - cite as `path (page X)` where X is 1-based for humans

6. If evidence is weak:
   - run another probe round (different synonyms / fewer constraints / OR-mode/raw FTS)
   - if still unclear, ask one targeted clarifying question (name, approximate date, doc type, folder, etc.)

### Rules
- Do not guess. If you can’t quote supporting text from retrieved pages, say so and continue searching or ask for clarification.
- Prefer reading a few pages thoroughly over skimming many snippets.

## Capability B: Propose and apply renames for timestamp-like filenames

### Intent
Some files have non-descriptive names (often just a date/time). Use Ringbinder’s OCR text to propose better filenames, then (only after confirmation) rename files and update Ringbinder’s paths.

### Procedure
1. Identify rename candidates:
   - basename looks like a timestamp (e.g. `20240224_123735.pdf`, `2024-02-24 12.37.35.jpg`, etc.).

2. For each candidate, read enough OCR to name it:
   - start with `ringbinder read --json --path <path> --page 0 --context 1`
   - if page 0 is uninformative, read more pages until naming is justified

3. Extract a relevant date and normalize it:
   - prefer a date that is *semantically primary* for the document (invoice date, statement period end, appointment date, letter date, etc.)
   - if no reliable OCR date exists, fallback to a date derived from the original filename
   - date formats must be one of:
     - `YYYY-MM-DD` (preferred when day is known)
     - `YYYY-MM` (when only month is known)
     - `YYYY` (when only year is known)

4. Propose a descriptive title from OCR:
   - short, specific summary: issuer/vendor/person + doc type + key subject
   - filesystem-safe (avoid `/`, `:`, control chars); keep it reasonably short

5. Build the proposed filename:
   - `<date> - <title>.<ext>`
   - example: `2024-01-17 - Aetna - Claim Denial Letter.pdf`
   - ensure uniqueness in the directory (add a disambiguator like ` (2)` only if needed)

6. Present one full rename plan and ask for confirmation:
   - show a single list of `OLD_PATH  ->  NEW_PATH`
   - ask the user to confirm before making any changes

7. After confirmation: perform renames and update Ringbinder:
   - rename files on disk
   - update Ringbinder’s stored paths afterwards so search results reflect new names
     - if Ringbinder has a dedicated rename command, use it
     - otherwise run `ringbinder sweep` on the affected roots; it should re-link by checksum without re-OCR

### Rules
- Never rename anything until the user explicitly confirms the full proposed list.
- Every proposed name must include a normalized date; only fallback to filename date when OCR doesn’t contain a reliable one.
- Do not invent titles; base them on text you actually read via `ringbinder read`.
