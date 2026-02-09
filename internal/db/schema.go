package db

const schemaVersion = 3

const schemaSQL = `
CREATE TABLE IF NOT EXISTS documents (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    path        TEXT    NOT NULL UNIQUE,
    checksum    TEXT    NOT NULL,
    created_at  TEXT    NOT NULL,
    modified_at TEXT    NOT NULL,
    page_count  INTEGER NOT NULL DEFAULT 1,
    ocr_pending INTEGER NOT NULL DEFAULT 1,
    deleted     INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_documents_ocr_pending
    ON documents(ocr_pending) WHERE deleted = 0;

CREATE INDEX IF NOT EXISTS idx_documents_path
    ON documents(path);

CREATE TABLE IF NOT EXISTS pages (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    page_index  INTEGER NOT NULL,
    markdown    TEXT    NOT NULL DEFAULT '',
    UNIQUE(document_id, page_index)
);

CREATE VIRTUAL TABLE IF NOT EXISTS pages_fts USING fts5(
    markdown,
    content='pages',
    content_rowid='id'
);

CREATE TRIGGER IF NOT EXISTS pages_ai AFTER INSERT ON pages BEGIN
    INSERT INTO pages_fts(rowid, markdown) VALUES (new.id, new.markdown);
END;

CREATE TRIGGER IF NOT EXISTS pages_ad AFTER DELETE ON pages BEGIN
    INSERT INTO pages_fts(pages_fts, rowid, markdown) VALUES('delete', old.id, old.markdown);
END;

CREATE TRIGGER IF NOT EXISTS pages_au AFTER UPDATE ON pages BEGIN
    INSERT INTO pages_fts(pages_fts, rowid, markdown) VALUES('delete', old.id, old.markdown);
    INSERT INTO pages_fts(rowid, markdown) VALUES (new.id, new.markdown);
END;
`
