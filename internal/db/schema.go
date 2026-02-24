package db

const schemaVersion = 2

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
