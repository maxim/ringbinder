package db

const schemaVersion = 4

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

CREATE TABLE IF NOT EXISTS gemini_batch_pages (
    request_id  INTEGER NOT NULL REFERENCES gemini_batch_requests(id) ON DELETE CASCADE,
    content_id  INTEGER NOT NULL REFERENCES contents(id) ON DELETE CASCADE,
    page_index  INTEGER NOT NULL CHECK (page_index >= 0),
    markdown    TEXT    NOT NULL,
    PRIMARY KEY(request_id, page_index),
    UNIQUE(content_id, page_index)
);

CREATE INDEX IF NOT EXISTS idx_gemini_batch_pages_content
    ON gemini_batch_pages(content_id, page_index);

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

const schemaV2ToV3SQL = `
CREATE TABLE gemini_batches (
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
    CHECK (state NOT IN ('uploaded', 'submission_unknown') OR input_file_name IS NOT NULL),
    CHECK (state NOT IN ('pending', 'running', 'cancelling', 'succeeded',
                         'failed', 'cancelled', 'expired') OR remote_name IS NOT NULL)
);

CREATE INDEX idx_gemini_batches_state ON gemini_batches(state);

CREATE TABLE gemini_batch_requests (
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

CREATE INDEX idx_gemini_requests_batch ON gemini_batch_requests(batch_id);
CREATE INDEX idx_gemini_requests_content ON gemini_batch_requests(content_id);
CREATE INDEX idx_gemini_requests_state ON gemini_batch_requests(state);

CREATE TABLE gemini_batch_pages (
    request_id  INTEGER NOT NULL REFERENCES gemini_batch_requests(id) ON DELETE CASCADE,
    content_id  INTEGER NOT NULL REFERENCES contents(id) ON DELETE CASCADE,
    page_index  INTEGER NOT NULL CHECK (page_index >= 0),
    markdown    TEXT    NOT NULL,
    PRIMARY KEY(request_id, page_index),
    UNIQUE(content_id, page_index)
);

CREATE INDEX idx_gemini_batch_pages_content
    ON gemini_batch_pages(content_id, page_index);

CREATE TABLE gemini_batch_cleanup (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    resource_kind  TEXT    NOT NULL CHECK (resource_kind IN ('file', 'batch')),
    resource_name  TEXT    NOT NULL,
    last_error     TEXT    NOT NULL DEFAULT '',
    created_at     TEXT    NOT NULL,
    updated_at     TEXT    NOT NULL,
    UNIQUE(resource_kind, resource_name)
);
`

const schemaV3ToV4SQL = `
ALTER TABLE gemini_batches ADD COLUMN content_provenance_complete INTEGER NOT NULL DEFAULT 1
    CHECK (content_provenance_complete IN (0, 1));

CREATE TABLE gemini_batch_contents (
    batch_id   INTEGER NOT NULL REFERENCES gemini_batches(id) ON DELETE CASCADE,
    content_id INTEGER NOT NULL REFERENCES contents(id) ON DELETE CASCADE,
    PRIMARY KEY(batch_id, content_id)
);

CREATE INDEX idx_gemini_batch_contents_content
    ON gemini_batch_contents(content_id);

INSERT OR IGNORE INTO gemini_batch_contents (batch_id, content_id)
-- Requests still assigned to their current batch.
SELECT b.id, r.content_id
FROM gemini_batches b
JOIN gemini_batch_requests r ON r.batch_id = b.id
UNION
-- Retryable, blocked, or split requests detached from their previous batch.
SELECT b.id, r.content_id
FROM gemini_batches b
JOIN gemini_batch_requests r ON r.previous_batch_id = b.id
UNION
-- Staged requests detach without recording previous_batch_id, but retain keys.
SELECT b.id, r.content_id
FROM gemini_batches b
CROSS JOIN json_each(b.request_keys) k
JOIN gemini_batch_requests r ON r.request_key = k.value;

-- A missing original key may represent promoted or orphaned content whose
-- identity v3 no longer retains. Split descendants are also conservative: an
-- incomplete flag is safer than erasing content that cannot be reconstructed.
UPDATE gemini_batches
SET content_provenance_complete = 0
WHERE EXISTS (
    SELECT 1
    FROM json_each(gemini_batches.request_keys) k
    WHERE NOT EXISTS (
        SELECT 1 FROM gemini_batch_requests r WHERE r.request_key = k.value
    )
);
`

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
