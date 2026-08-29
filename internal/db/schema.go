package db

const schemaVersion = 5

const schemaSQL = `
CREATE TABLE IF NOT EXISTS contents (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    checksum    TEXT    NOT NULL UNIQUE,
    page_count  INTEGER NOT NULL DEFAULT 1,
    ocr_pending INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS documents (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    path        TEXT    NOT NULL UNIQUE,
    content_id  INTEGER NOT NULL REFERENCES contents(id),
    created_at  TEXT    NOT NULL,
    modified_at TEXT    NOT NULL,
    deleted     INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_contents_ocr_pending
    ON contents(ocr_pending);

CREATE INDEX IF NOT EXISTS idx_documents_path
    ON documents(path);

CREATE INDEX IF NOT EXISTS idx_documents_content_id
    ON documents(content_id) WHERE deleted = 0;

CREATE TABLE IF NOT EXISTS pages (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    content_id  INTEGER NOT NULL REFERENCES contents(id) ON DELETE CASCADE,
    page_index  INTEGER NOT NULL,
    markdown    TEXT    NOT NULL DEFAULT '',
    search_text TEXT    NOT NULL DEFAULT '',
    model       TEXT,
    UNIQUE(content_id, page_index)
);

CREATE VIRTUAL TABLE IF NOT EXISTS pages_fts USING fts5(
    search_text,
    content='pages',
    content_rowid='id'
);

CREATE VIRTUAL TABLE IF NOT EXISTS pages_fts_trigram USING fts5(
    search_text,
    tokenize='trigram',
    content='pages',
    content_rowid='id'
);

CREATE TRIGGER IF NOT EXISTS pages_ai AFTER INSERT ON pages BEGIN
    INSERT INTO pages_fts(rowid, search_text) VALUES (new.id, new.search_text);
    INSERT INTO pages_fts_trigram(rowid, search_text) VALUES (new.id, new.search_text);
END;

CREATE TRIGGER IF NOT EXISTS pages_ad AFTER DELETE ON pages BEGIN
    INSERT INTO pages_fts(pages_fts, rowid, search_text) VALUES('delete', old.id, old.search_text);
    INSERT INTO pages_fts_trigram(pages_fts_trigram, rowid, search_text) VALUES('delete', old.id, old.search_text);
END;

CREATE TRIGGER IF NOT EXISTS pages_au AFTER UPDATE ON pages BEGIN
    INSERT INTO pages_fts(pages_fts, rowid, search_text) VALUES('delete', old.id, old.search_text);
    INSERT INTO pages_fts(rowid, search_text) VALUES (new.id, new.search_text);

    INSERT INTO pages_fts_trigram(pages_fts_trigram, rowid, search_text) VALUES('delete', old.id, old.search_text);
    INSERT INTO pages_fts_trigram(rowid, search_text) VALUES (new.id, new.search_text);
END;

` + geminiBatchSchemaSQL

const geminiBatchSchemaSQL = `
CREATE TABLE IF NOT EXISTS gemini_batches (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    display_name      TEXT    NOT NULL UNIQUE,
    model             TEXT    NOT NULL,
    request_keys      TEXT    NOT NULL CHECK (json_valid(request_keys)),
    state             TEXT    NOT NULL CHECK (state IN (
        'prepared', 'upload_unknown', 'uploaded', 'submission_unknown',
        'pending', 'running', 'cancelling', 'succeeded', 'failed',
        'cancelled', 'expired'
    )),
    input_file_name   TEXT,
    output_file_name  TEXT,
    remote_name       TEXT    UNIQUE,
    input_price       INTEGER NOT NULL CHECK (input_price >= 0),
    output_price      INTEGER NOT NULL CHECK (output_price >= 0),
    replacement_of    INTEGER,
    last_error        TEXT    NOT NULL DEFAULT '',
    created_at        TEXT    NOT NULL,
    updated_at        TEXT    NOT NULL,
    content_provenance_complete INTEGER NOT NULL DEFAULT 1
        CHECK (content_provenance_complete IN (0, 1)),
    CHECK (state NOT IN ('uploaded', 'submission_unknown') OR input_file_name IS NOT NULL),
    CHECK (state NOT IN ('pending', 'running', 'cancelling', 'succeeded',
                         'failed', 'cancelled', 'expired') OR remote_name IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS idx_gemini_batches_state
    ON gemini_batches(state);

CREATE TABLE IF NOT EXISTS gemini_batch_contents (
    batch_id   INTEGER NOT NULL REFERENCES gemini_batches(id) ON DELETE CASCADE,
    content_id INTEGER NOT NULL REFERENCES contents(id) ON DELETE CASCADE,
    PRIMARY KEY(batch_id, content_id)
);

CREATE INDEX IF NOT EXISTS idx_gemini_batch_contents_content
    ON gemini_batch_contents(content_id);

CREATE TABLE IF NOT EXISTS gemini_batch_requests (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    content_id         INTEGER NOT NULL REFERENCES contents(id) ON DELETE CASCADE,
    batch_id           INTEGER REFERENCES gemini_batches(id),
    request_key        TEXT    NOT NULL UNIQUE,
    file_type          TEXT    NOT NULL CHECK (file_type IN ('pdf', 'jpeg', 'png')),
    page_start         INTEGER NOT NULL CHECK (page_start >= 0),
    page_end           INTEGER NOT NULL CHECK (page_end > page_start),
    state              TEXT    NOT NULL CHECK (state IN ('assigned', 'staged', 'retryable', 'blocked')),
    attempt_count      INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count BETWEEN 0 AND 1),
    replacement_count  INTEGER NOT NULL DEFAULT 0 CHECK (replacement_count BETWEEN 0 AND 1),
    previous_batch_id  INTEGER,
    split_depth        INTEGER NOT NULL DEFAULT 0 CHECK (split_depth >= 0),
    input_tokens       INTEGER CHECK (input_tokens >= 0),
    output_tokens      INTEGER CHECK (output_tokens >= 0),
    known_cost         INTEGER NOT NULL DEFAULT 0 CHECK (known_cost >= 0),
    cost_indeterminate INTEGER NOT NULL DEFAULT 0 CHECK (cost_indeterminate IN (0, 1)),
    last_error         TEXT    NOT NULL DEFAULT '',
    created_at         TEXT    NOT NULL,
    updated_at         TEXT    NOT NULL,
    UNIQUE(content_id, page_start, page_end),
    CHECK ((state = 'assigned' AND batch_id IS NOT NULL) OR
           (state != 'assigned' AND batch_id IS NULL))
);

CREATE INDEX IF NOT EXISTS idx_gemini_requests_batch
    ON gemini_batch_requests(batch_id);
CREATE INDEX IF NOT EXISTS idx_gemini_requests_content
    ON gemini_batch_requests(content_id);
CREATE INDEX IF NOT EXISTS idx_gemini_requests_state
    ON gemini_batch_requests(state);

CREATE TABLE IF NOT EXISTS gemini_batch_cleanup (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    resource_kind  TEXT    NOT NULL CHECK (resource_kind IN ('file', 'batch')),
    resource_name  TEXT    NOT NULL,
    last_error     TEXT    NOT NULL DEFAULT '',
    created_at     TEXT    NOT NULL,
    updated_at     TEXT    NOT NULL,
    UNIQUE(resource_kind, resource_name)
);
`

// v3 and v4 were unreleased development schemas. The only supported public
// upgrade input after v1 composition is the exact v0.2.0 schema v2.
const schemaV2ToV5SQL = `
ALTER TABLE pages ADD COLUMN model TEXT;
-- Released v2 could mark changed content complete without copying its pages, so
-- legacy flags must be reconciled from exact canonical coverage.
UPDATE contents
SET ocr_pending = CASE WHEN (
    SELECT COUNT(*) FROM pages p
    WHERE p.content_id = contents.id
      AND p.page_index >= 0
      AND p.page_index < contents.page_count
) = page_count THEN 0 ELSE 1 END;
` + geminiBatchSchemaSQL

const schemaV1ToV2SQL = `
ALTER TABLE pages ADD COLUMN search_text TEXT NOT NULL DEFAULT '';
UPDATE pages SET search_text = markdown;

DROP TRIGGER IF EXISTS pages_ai;
DROP TRIGGER IF EXISTS pages_ad;
DROP TRIGGER IF EXISTS pages_au;
DROP TABLE IF EXISTS pages_fts;

CREATE VIRTUAL TABLE IF NOT EXISTS pages_fts USING fts5(
    search_text,
    content='pages',
    content_rowid='id'
);

CREATE VIRTUAL TABLE IF NOT EXISTS pages_fts_trigram USING fts5(
    search_text,
    tokenize='trigram',
    content='pages',
    content_rowid='id'
);

CREATE TRIGGER IF NOT EXISTS pages_ai AFTER INSERT ON pages BEGIN
    INSERT INTO pages_fts(rowid, search_text) VALUES (new.id, new.search_text);
    INSERT INTO pages_fts_trigram(rowid, search_text) VALUES (new.id, new.search_text);
END;

CREATE TRIGGER IF NOT EXISTS pages_ad AFTER DELETE ON pages BEGIN
    INSERT INTO pages_fts(pages_fts, rowid, search_text) VALUES('delete', old.id, old.search_text);
    INSERT INTO pages_fts_trigram(pages_fts_trigram, rowid, search_text) VALUES('delete', old.id, old.search_text);
END;

CREATE TRIGGER IF NOT EXISTS pages_au AFTER UPDATE ON pages BEGIN
    INSERT INTO pages_fts(pages_fts, rowid, search_text) VALUES('delete', old.id, old.search_text);
    INSERT INTO pages_fts(rowid, search_text) VALUES (new.id, new.search_text);

    INSERT INTO pages_fts_trigram(pages_fts_trigram, rowid, search_text) VALUES('delete', old.id, old.search_text);
    INSERT INTO pages_fts_trigram(rowid, search_text) VALUES (new.id, new.search_text);
END;

INSERT INTO pages_fts(pages_fts) VALUES('rebuild');
INSERT INTO pages_fts_trigram(pages_fts_trigram) VALUES('rebuild');
`
