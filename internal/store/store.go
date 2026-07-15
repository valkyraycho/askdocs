package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/ncruces/go-sqlite3"
	_ "github.com/ncruces/go-sqlite3/driver"
)

const (
	applicationID      = 0x41534B44 // "ASKD"
	baseSchemaVersion  = 1
	vecSchemaVersion   = 2
	filePerm           = 0o600
	migrateRetryWindow = 5 * time.Second
	migrateRetryDelay  = 50 * time.Millisecond
)

var (
	ErrNotFound     = errors.New("not found")
	ErrNoEmbeddings = errors.New("corpus has no embeddings yet — run askdocs ingest")
)

const baseSchema = `
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE files (
  path        TEXT PRIMARY KEY,
  hash        TEXT NOT NULL,
  ingested_at TEXT NOT NULL
);
CREATE TABLE chunks (
  id      INTEGER PRIMARY KEY AUTOINCREMENT,
  path    TEXT NOT NULL REFERENCES files(path) ON DELETE CASCADE,
  heading TEXT NOT NULL,
  content TEXT NOT NULL,
  pos     INTEGER NOT NULL
);
CREATE INDEX idx_chunks_path ON chunks(path);
CREATE VIRTUAL TABLE chunks_fts USING fts5(heading, content, tokenize='porter unicode61');
`

type Store struct {
	db       *sql.DB
	writable bool
}

func OpenIngest(path string) (*Store, error) {
	dsn := "file:" + path +
		"?_txlock=immediate" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := smokeTestVec(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := os.Chmod(path, filePerm); err != nil {
		db.Close()
		return nil, fmt.Errorf("restrict database permissions: %w", err)
	}
	return &Store{db: db, writable: true}, nil
}

func OpenReadOnly(path string) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no corpus at %s — run askdocs ingest (%w)", path, err)
	}
	dsn := "file:" + path + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := smokeTestVec(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := verify(db, path); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, writable: false}, nil
}

func (s *Store) Close() error {
	if s.writable {
		s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)") // best-effort; BUSY is acceptable
	}
	return s.db.Close()
}

func smokeTestVec(db *sql.DB) error {
	var version string
	if err := db.QueryRow("SELECT vec_version()").Scan(&version); err != nil {
		return fmt.Errorf("sqlite-vec is not available in this build: %w", err)
	}
	return nil
}

func verify(db *sql.DB, path string) error {
	var appID int64
	if err := db.QueryRow("PRAGMA application_id").Scan(&appID); err != nil {
		return fmt.Errorf("read application_id: %w", err)
	}
	if appID != applicationID {
		return fmt.Errorf("%s is not an askdocs corpus (application_id %#x)", path, appID)
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version < baseSchemaVersion {
		return fmt.Errorf("%s has no askdocs schema — run askdocs ingest", path)
	}
	if version > vecSchemaVersion {
		return fmt.Errorf("corpus schema version %d is newer than this askdocs supports", version)
	}
	return nil
}

func migrate(db *sql.DB) error {
	deadline := time.Now().Add(migrateRetryWindow)
	for {
		err := migrateOnce(db)
		if err == nil || !isBusy(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(migrateRetryDelay)
	}
}

func isBusy(err error) bool {
	var serr *sqlite3.Error
	return errors.As(err, &serr) && serr.Code() == sqlite3.BUSY
}

func migrateOnce(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer tx.Rollback()

	var appID int64
	if err := tx.QueryRow("PRAGMA application_id").Scan(&appID); err != nil {
		return fmt.Errorf("read application_id: %w", err)
	}
	var version int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	switch {
	case appID == 0 && version == 0:
		// fresh (or pre-schema) database: claim and initialize it below
	case appID != applicationID:
		return fmt.Errorf("existing file is not an askdocs corpus (application_id %#x)", appID)
	case version > vecSchemaVersion:
		return fmt.Errorf("corpus schema version %d is newer than this askdocs supports", version)
	default:
		return tx.Commit()
	}

	var pageCount int64
	if err := tx.QueryRow("PRAGMA page_count").Scan(&pageCount); err != nil {
		return fmt.Errorf("read page_count: %w", err)
	}
	if pageCount > 1 {
		return errors.New("existing file contains foreign data — refusing to initialize an askdocs corpus over it")
	}
	if _, err := tx.Exec(baseSchema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA application_id = %d", applicationID)); err != nil {
		return fmt.Errorf("stamp application_id: %w", err)
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", baseSchemaVersion)); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}
	return tx.Commit()
}
