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
    UNIQUE(content_id, page_index)
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
