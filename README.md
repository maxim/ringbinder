# Ringbinder

Ringbinder makes PDFs and images on your computer searchable. It uses vision LLMs (like Gemini and Mistral) to read them and stores the resulting text in a local SQLite database. Your original files stay where they are and are never modified.

Ringbinder supports Gemini Flash and Mistral OCR, staying up to date with their latest versions. At this time Gemini's quality is better, but their policy can refuse to process some documents, which is why Ringbinder supports fallback to another provider.

There are two ways to run OCR:

- `ringbinder ocr` processes documents now and waits for the result. This uses the normal price.
- `ringbinder batch` (Gemini only) sends batches of work at half price and collects the results later.

## Features

- Indexes PDF, PNG, JPEG, and JPG files
- Uses Gemini or Mistral for OCR
- Can try a second OCR service when the first one says it cannot process a page
- Offers half-price Gemini OCR for work that can finish later
- Saves every successful page, so interrupted work can resume without starting over
- Searches OCR text and filenames, including fuzzy matches for messy scans
- Reads individual pages, groups of pages, or nearby pages for context
- Produces JSON for scripts and agents
- Recognizes identical copies of a file and OCRs them only once
- Tracks files that were added, changed, removed, or restored

## Install

Ringbinder runs on macOS and Linux. Install the released version with Homebrew:

```sh
brew install maxim/tap/ringbinder
```

With Go installed, install the latest source directly:

```sh
go install github.com/maxim/ringbinder@latest
```

Or build from a clone:

```sh
git clone https://github.com/maxim/ringbinder.git
cd ringbinder
go build -o ringbinder .
```

For Gemini OCR, set:

```sh
export GEMINI_API_KEY="..."
```

Set a Mistral key when your config includes Mistral, or when `model` is not set and Ringbinder uses its Mistral default:

```sh
export MISTRAL_API_KEY="..."
```

## Quick start

Create a config file. This example scans two locations and uses Gemini:

```sh
mkdir -p ~/.config/ringbinder
cat > ~/.config/ringbinder/config.yml <<'YAML'
paths:
  - ~/Documents
  - ~/Downloads/**/*.pdf
model: gemini
YAML
```

Then run the usual loop:

```sh
# Find new, changed, and missing files.
ringbinder sweep

# Estimate the regular OCR price without contacting Gemini.
ringbinder cost

# OCR unfinished documents now.
ringbinder ocr

# Search the finished OCR text.
ringbinder find "property tax" --verbose

# Read a page from a result. Page numbers in flags start at 0.
ringbinder read --path "/Users/you/Documents/taxes/assessment.pdf" --page 0 --context 1
```

The database defaults to `~/.config/ringbinder/ringbinder.db`. Use `database_path` in config or the global `--database` flag to put it somewhere else.

## How your data is stored

Ringbinder keeps file locations and OCR text in one local SQLite database. Original documents are never copied into it.

- Identical files at several locations share one set of OCR pages, so they are processed only once.
- Every successful page is saved immediately. A failure on another page does not erase good work.
- A document stays out of normal text search and `read` until every page is finished. You can still find it by filename and see that OCR is waiting.
- Ringbinder records the service and model used for each page. Older OCR results may show `unknown` because this information was not recorded at the time.
- Missing or excluded files remain in the database so Ringbinder can restore them if they reappear.

## Configuration

Ringbinder reads:

```text
~/.config/ringbinder/config.yml
```

A typical config looks like this:

```yaml
paths:
  - ~/Documents
  - ~/Downloads/**/*.pdf

# Gemini is recommended for the best OCR results.
model: gemini

# Never include these files in a sweep.
exclude:
  - "**/private/*.pdf"
  - "draft-scan.pdf"

# Optional. Defaults to 4.
sweep_concurrency: 8
```

### Choosing OCR services

Set `model` to `gemini` or `mistral`. Gemini is recommended. When the setting is absent, Ringbinder uses Mistral so older configs continue working as before.

You can also list two services in the order you want them tried:

```yaml
model:
  - gemini
  - mistral
```

Gemini handles each page first in this example. Ringbinder tries Mistral only when Gemini clearly says it cannot process that page. A timeout or temporary service problem leaves the page for a later run instead of sending it elsewhere immediately.

You can make the same choice for one command:

```sh
ringbinder cost --model gemini
ringbinder ocr --model gemini
ringbinder ocr --model gemini --model mistral --limit 100
```

The `model` setting accepts one service or an ordered list. Repeat `--model` to set that order for one command.

### Excluding files

Config exclusions apply to every sweep. Command-line exclusions add more for one run; they never remove the configured exclusions.

```sh
ringbinder sweep ~/Documents --exclude "**/inbox/*.pdf" --exclude "draft-scan.pdf,*.tmp.pdf"
```

Excluded files disappear from normal OCR and search results after the next sweep. Remove the exclusion and sweep again to restore them. If an OCR service repeatedly refuses a document, excluding every copy of that file keeps Ringbinder from trying it again.

### Other settings

`sweep_concurrency` controls how many files Ringbinder examines at once. It defaults to 4. Override it with `sweep --concurrency`.

Set a custom database location with:

```yaml
database_path: ~/.local/share/ringbinder/ringbinder.db
paths:
  - ~/Documents
```

Paths can also be supplied on the command line. `--config` chooses another config file, and `--database` chooses another database:

```sh
ringbinder sweep ~/Documents ~/Desktop/inbox
ringbinder --config ./ringbinder.yml sweep
ringbinder --database ./ringbinder.db find "property tax"
```

File patterns such as `*.pdf` and `**/*.pdf` are supported.

## Commands

### `sweep`

Find new, changed, missing, and restored files:

```sh
ringbinder sweep [paths...]
```

Useful flags:

- `--exclude <pattern>` skips matching files; repeat it or use comma-separated patterns
- `-j, --concurrency <n>` changes how many files are examined at once

A sweep marks missing and excluded files only inside the locations scanned by that command. If a file comes back, the next sweep restores it. OCR is requested again only when both the file contents and its modification time changed.

### `cost`

`ringbinder cost` estimates the price of running `ringbinder ocr`. It does not contact Gemini or Mistral, needs no API key, and changes nothing in the database.

```sh
ringbinder cost --model gemini
ringbinder cost --model gemini --model mistral
ringbinder cost --limit 100
```

The estimate includes only unfinished pages. Identical copies of a file count once. Documents already being handled by a Gemini batch job are left out.

The low estimate assumes the first selected service handles every unfinished page. The high estimate assumes every selected service has to try every unfinished page, then adds 5% for retries. Actual prices can be higher when responses are unusually long or extra requests are needed.

Gemini pricing depends on how much data is sent and returned, so its estimate is approximate. Mistral is estimated at the built-in annotated-page price of `$0.0050/page`. “Annotated page” is Mistral's price tier for OCR that also describes images and graphics, which Ringbinder requests to make visual material searchable.

For Gemini's half-price Batch API, use the separate estimate:

```sh
ringbinder batch cost --limit 100
```

`--limit <n>` estimates the next `n` documents. Identical copies still count once.

### `ocr`

`ringbinder ocr` processes unfinished pages now and waits for the results. This uses the normal price. Gemini is recommended for the best results.

```sh
ringbinder ocr --model gemini
ringbinder ocr --model gemini --model mistral
ringbinder ocr --limit 100
```

Only unfinished pages are sent, and every successful page is saved immediately. `--limit <n>` restricts the run to the next `n` documents. A failed document still counts toward that limit.

When two services are listed, Ringbinder starts with the first one. If that service clearly says it cannot process a page, Ringbinder tries the next one. It may break a large group of pages into smaller groups to save the pages that still work.

Temporary errors do not switch services. Those pages wait for the next run, while Ringbinder continues with other documents. Account, billing, permission, and API-key errors stop the run so they can be fixed.

A document does not appear in normal text search or `read` until every page is ready. Successfully finished pages remain saved in the meantime.

At the end, Ringbinder reports how many pages finished, which service handled them, what remains, and the known price of the work attempted. It warns when the service did not return enough billing information to know the full amount.

If some pages are already part of a running Gemini batch job, `ringbinder ocr` leaves that document alone to avoid duplicate work and charges.

### `batch`

Gemini's Batch API costs half the regular Gemini price and finishes later. Use it when saving money matters more than getting the result immediately. Batch commands always use Gemini; the `model` setting does not affect them.

These commands contact Gemini and require `GEMINI_API_KEY`:

- `batch start`
- `batch continue`
- `batch list`
- `batch cancel`
- `batch retry`

These commands stay local and need no key:

- `batch cost`
- `batch failures`
- `batch forget`

A typical batch workflow is:

```sh
# Estimate the half-price batch cost without contacting Gemini.
ringbinder batch cost --limit 100

# Submit unfinished pages and return without waiting for OCR.
ringbinder batch start --limit 100

# Check saved jobs, download finished pages, and continue recovery work.
ringbinder batch continue

# View current jobs.
ringbinder batch list
ringbinder batch list --json

# Stop or forget a job.
ringbinder batch cancel 17
ringbinder batch forget 17

# See pages that need help and retry one group now at the regular Gemini price.
ringbinder batch failures
ringbinder batch failures --json
ringbinder batch retry 83 --mode direct
```

`batch cost` and `batch start` include only unfinished pages that are not already part of another batch job. Their `--limit` counts documents, with identical copies counted once. A large submission may be split into several jobs.

Run `batch continue` periodically. It checks saved jobs, downloads available results, retries recoverable failures, and cleans up uploaded files. It also resumes safely after an interruption or reboot. It does not add newly scanned documents; run `batch start` again for those.

Every successful page is saved immediately. A partially finished document remains out of normal text search and `read` until its remaining pages finish.

When `batch failures` reports pages that need attention, you can retry one group immediately:

```sh
ringbinder batch retry <request-id> --mode direct
```

Here, `--mode direct` means “run this retry now at the regular Gemini price” rather than sending it through the half-price Batch API again.

You can also let the normal OCR command finish pages that are no longer part of a running batch. This example uses Gemini first and tries Mistral only for pages Gemini says it cannot process:

```sh
ringbinder ocr --model gemini --model mistral
```

`batch cancel` asks Gemini to stop a job. Cancellation may take time, so keep running `batch continue` until the job is settled.

`batch forget <id>` removes Ringbinder's record of unfinished work for that job. OCR pages that already succeeded are kept. Ringbinder will also stop checking or cleaning up that job on Gemini.

Only one command that changes OCR or batch jobs can use a database at a time. Search, reading, estimates, and failure lists remain available.

#### Common batch situations

| Situation | What to do |
|---|---|
| Ringbinder stops, the network goes offline, or a request times out | Restore connectivity and run `ringbinder batch continue`. Saved progress remains in SQLite. |
| Ringbinder cannot tell whether Gemini accepted a job | Run `ringbinder batch continue` later. Ringbinder looks for the matching job before submitting anything again. Use `batch forget <id>` only when you want Ringbinder to stop managing it. |
| A file changes, moves, or disappears | Run `ringbinder sweep`, then `ringbinder batch continue`. Ringbinder checks the file before saving OCR results. |
| Only some pages succeed | Successful pages stay saved. Run `batch continue`; pages that still need attention eventually appear under `batch failures`. |
| A job is cancelled, expires, or finishes without usable output | Run `batch continue`, then follow any recovery command shown by `batch failures`. |

### `find`

Search finished OCR text and filenames:

```sh
ringbinder find "insurance claim"
ringbinder find "insurance claim" --verbose
ringbinder find "insurance claim" --mode or
ringbinder find "insur claim" --trigram
ringbinder find --fts '"insurance" AND "claim"'
```

Useful flags:

- `--mode and|or` controls whether every search word must match
- `--fts <query>` accepts an advanced SQLite full-text search expression
- `--trigram` also looks for fuzzy or partial text, which helps with OCR mistakes
- `--limit` and `--offset` move through large result sets
- `--json` prints one JSON record per result

`--verbose` also shows the OCR service and whether the document is still waiting for pages. Filename-only results can include unfinished documents.

### `read`

Read OCR text for one page or several pages:

```sh
ringbinder read --path "/path/to/file.pdf" --page 3
ringbinder read --path "/path/to/file.pdf" --page 3 --context 1
ringbinder read --path "/path/to/file.pdf" --start 0 --end 4
ringbinder read --path "/path/to/file.pdf" --page 3 --json
```

Page flags start at 0. Human-readable output displays pages starting at 1 and shows the service and model used for each page. Older results show `unknown`. `read` returns no OCR text while any page in the document is unfinished.

### `doc`

List indexed documents or inspect one document:

```sh
ringbinder doc list
ringbinder doc list --after 2026-01-01 --before 2026-02-01
ringbinder doc list --json
ringbinder doc get --path "/path/to/file.pdf"
ringbinder doc get --path "/path/to/file.pdf" --json
```

Human-readable output shows how many pages are finished and how many pages each OCR service handled. See [Automation](#automation) for the JSON fields.

## Rebuilding all OCR

To rerun every document with a different OCR service, create a second database. Your current database remains searchable until the replacement is ready.

```sh
new_db="$HOME/.config/ringbinder/ringbinder-new.db"

ringbinder --database "$new_db" sweep
ringbinder --database "$new_db" cost --model gemini --limit 100
ringbinder --database "$new_db" ocr --model gemini --limit 100

ringbinder --database "$new_db" cost --model gemini
ringbinder --database "$new_db" ocr --model gemini
```

Check a small sample first. When the results look good, process the rest, run one final `sweep`, and finish any newly found pages. Then set `database_path` to the new database.

## Automation

Use `--json` when another program or agent needs the results:

```sh
ringbinder find --json --limit 10 "lease renewal"
ringbinder read --json --path "/path/to/lease.pdf" --page 2 --context 1
ringbinder doc list --json --limit 100
ringbinder doc get --json --path "/path/to/lease.pdf"
ringbinder batch list --json
ringbinder batch failures --json
```

`find --json` prints one record per result. Important fields include:

- `path`, `page_index`, `page_count`, `snippet`, `rank`, and `search_source`
- `model`: the service and model used for an OCR text result; `null` for a filename-only result
- `ocr_pending`: `true` while the document still has unfinished pages

`read --json` prints one object with `path` and `pages`. Each page has `page_index`, `markdown`, and `model`. The model is `null` for older results where it was not recorded.

`doc get --json` prints one object. `doc list --json` prints one object per line. Each object has `path`, `created_at`, `modified_at`, `page_count`, `ocr_pages_completed`, `models`, `ocr_pending`, and `deleted`. `models` counts pages by service and model; older pages are grouped under `"model": null`.

`batch failures --json` prints one record for each failed group of pages. Each record has `request_id`, `paths`, `page_start`, `page_end`, `attempt_count`, and `error`. Page numbers start at 1 here. `attempt_count` counts automatic retries after the original attempt.

`batch list --json` prints current jobs, pages that need attention, cleanup still to do, and refresh errors. A job with `"stale": true` could not be checked during that run. Details appear in `refresh_errors`.

The included `SKILL.md` is an example agent skill for finding and citing scanned documents.

## Privacy and storage

Ringbinder keeps its index and OCR text in a local SQLite database. By default that is `~/.config/ringbinder/ringbinder.db`; use `database_path` or `--database` to choose another location.

Ringbinder keeps a record of missing and excluded files so it can restore them later. Their OCR text therefore stays in the database after the files disappear or are excluded. Ringbinder has no command for deleting selected OCR records; create a fresh database when you need to remove retained text.

`ringbinder ocr` sends unfinished pages to the selected OCR service. When two services are listed, a page may be sent to the second one if the first says it cannot process it. `batch start` uploads work to Gemini, while `batch continue`, `list`, `cancel`, and `retry` may contact Gemini to manage saved jobs. `cost`, `batch cost`, `batch failures`, and `batch forget` stay local and offline.

For batch jobs, Ringbinder creates private temporary upload and download files and removes them after use. It does not store the original document bytes in the database. Successful OCR text is saved locally as soon as it arrives, even when other pages are unfinished. Ringbinder asks Gemini to delete uploaded files and finished job records; Gemini keeps generated output according to its own retention policy. `batch continue` retries cleanup when a temporary problem gets in the way.

If sending a folder's documents to an OCR service is unacceptable, exclude that folder from configuration and run `ringbinder sweep`.

## Development

```sh
go test ./...
go build -o ringbinder .
```

Project layout:

```text
cmd/        CLI commands
internal/   scanner, database, OCR, formatting, and support packages
main.go     entry point
```

## License

Ringbinder is released under the MIT License. See [LICENSE](LICENSE).
