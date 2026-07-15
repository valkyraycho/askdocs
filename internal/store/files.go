package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type Chunk struct {
	ID      int64
	Path    string
	Heading string
	Content string
	Pos     int
}

type ChunkInput struct {
	Heading string
	Content string
	Vec     []float32
}

type FileInfo struct {
	Path   string
	Chunks int
}

func (s *Store) Files() ([]FileInfo, error) {
	rows, err := s.db.Query(
		`SELECT f.path, count(c.id) FROM files f
		 LEFT JOIN chunks c ON c.path = f.path
		 GROUP BY f.path ORDER BY f.path`)
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}
	defer rows.Close()

	var files []FileInfo
	for rows.Next() {
		var f FileInfo
		if err := rows.Scan(&f.Path, &f.Chunks); err != nil {
			return nil, fmt.Errorf("scan file info: %w", err)
		}
		files = append(files, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("file info rows: %w", err)
	}
	return files, nil
}

func (s *Store) FileHashes() (map[string]string, error) {
	rows, err := s.db.Query("SELECT path, hash FROM files")
	if err != nil {
		return nil, fmt.Errorf("list file hashes: %w", err)
	}
	defer rows.Close()

	hashes := make(map[string]string)
	for rows.Next() {
		var path, hash string
		if err := rows.Scan(&path, &hash); err != nil {
			return nil, fmt.Errorf("scan file hash: %w", err)
		}
		hashes[path] = hash
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("file hash rows: %w", err)
	}
	return hashes, nil
}

// ReplaceFile atomically replaces a file's chunks, FTS rows, and vectors.
// It is only called once every chunk of the file has been embedded, so a
// mid-file failure upstream can never leave a partially replaced file.
func (s *Store) ReplaceFile(path, hash string, chunks []ChunkInput) error {
	if !s.writable {
		return errors.New("read-only store")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin replace %s: %w", path, err)
	}
	defer tx.Rollback()

	if err := deleteFileRows(tx, path); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.Exec("INSERT INTO files (path, hash, ingested_at) VALUES (?, ?, ?)", path, hash, now); err != nil {
		return fmt.Errorf("insert file %s: %w", path, err)
	}
	for pos, c := range chunks {
		res, err := tx.Exec("INSERT INTO chunks (path, heading, content, pos) VALUES (?, ?, ?, ?)",
			path, c.Heading, c.Content, pos)
		if err != nil {
			return fmt.Errorf("insert chunk %s#%d: %w", path, pos, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("chunk id %s#%d: %w", path, pos, err)
		}
		if _, err := tx.Exec("INSERT INTO chunks_fts (rowid, heading, content) VALUES (?, ?, ?)",
			id, c.Heading, c.Content); err != nil {
			return fmt.Errorf("index chunk %s#%d: %w", path, pos, err)
		}
		if c.Vec != nil {
			if _, err := tx.Exec("INSERT INTO chunks_vec (rowid, embedding) VALUES (?, ?)",
				id, serializeF32(c.Vec)); err != nil {
				return fmt.Errorf("embed chunk %s#%d: %w", path, pos, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit replace %s: %w", path, err)
	}
	return nil
}

func (s *Store) DeleteFiles(paths []string) error {
	if !s.writable {
		return errors.New("read-only store")
	}
	if len(paths) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin prune: %w", err)
	}
	defer tx.Rollback()

	for _, path := range paths {
		if err := deleteFileRows(tx, path); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit prune: %w", err)
	}
	return nil
}

func deleteFileRows(tx *sql.Tx, path string) error {
	rows, err := tx.Query("SELECT id FROM chunks WHERE path = ?", path)
	if err != nil {
		return fmt.Errorf("list chunks of %s: %w", path, err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan chunk id of %s: %w", path, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("chunk id rows of %s: %w", path, err)
	}
	rows.Close()

	hasVec := tableExists(tx, "chunks_vec")
	for _, id := range ids {
		if _, err := tx.Exec("DELETE FROM chunks_fts WHERE rowid = ?", id); err != nil {
			return fmt.Errorf("deindex chunk %d: %w", id, err)
		}
		if hasVec {
			if _, err := tx.Exec("DELETE FROM chunks_vec WHERE rowid = ?", id); err != nil {
				return fmt.Errorf("unembed chunk %d: %w", id, err)
			}
		}
	}
	if _, err := tx.Exec("DELETE FROM chunks WHERE path = ?", path); err != nil {
		return fmt.Errorf("delete chunks of %s: %w", path, err)
	}
	if _, err := tx.Exec("DELETE FROM files WHERE path = ?", path); err != nil {
		return fmt.Errorf("delete file %s: %w", path, err)
	}
	return nil
}

func tableExists(tx *sql.Tx, name string) bool {
	var n int
	if err := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type IN ('table','view') AND name = ?", name).Scan(&n); err != nil {
		return false
	}
	return n > 0
}
