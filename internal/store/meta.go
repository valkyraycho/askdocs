package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

type EmbeddingSpace struct {
	Model string
	Dims  int
	Host  string
}

func (s *Store) Space() (EmbeddingSpace, error) {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return EmbeddingSpace{}, fmt.Errorf("read schema version: %w", err)
	}
	if version < vecSchemaVersion {
		return EmbeddingSpace{}, ErrNoEmbeddings
	}
	sp := EmbeddingSpace{}
	var dims string
	for key, dst := range map[string]*string{
		"embed_model": &sp.Model, "embed_dims": &dims, "embed_provider_host": &sp.Host,
	} {
		if err := s.db.QueryRow("SELECT value FROM meta WHERE key = ?", key).Scan(dst); err != nil {
			return EmbeddingSpace{}, fmt.Errorf("read meta %s: %w", key, err)
		}
	}
	var err error
	if sp.Dims, err = strconv.Atoi(dims); err != nil {
		return EmbeddingSpace{}, fmt.Errorf("corrupt embed_dims: %w", err)
	}
	return sp, nil
}

// EnsureSpace performs phase-2 initialization on first call (creates the vec0
// table and stamps the embedding space) and enforces space identity on every
// later call. Model/dims mismatches have no override; a provider-host
// mismatch may be restamped when allowHostChange asserts space equivalence.
func (s *Store) EnsureSpace(want EmbeddingSpace, allowHostChange bool) error {
	if !s.writable {
		return errors.New("embedding space can only be initialized by ingest")
	}
	got, err := s.Space()
	if errors.Is(err, ErrNoEmbeddings) {
		return s.initSpace(want)
	}
	if err != nil {
		return err
	}
	if got.Model != want.Model || got.Dims != want.Dims {
		return fmt.Errorf("corpus was embedded with %s (%d dims); asked for %s (%d dims) — re-ingest into a fresh corpus",
			got.Model, got.Dims, want.Model, want.Dims)
	}
	if got.Host != want.Host {
		if !allowHostChange {
			return fmt.Errorf("corpus was embedded via %s, current endpoint is %s — set ASKDOCS_ALLOW_PROVIDER_MISMATCH=1 only if both serve the same embedding space",
				got.Host, want.Host)
		}
		if _, err := s.db.Exec("UPDATE meta SET value = ? WHERE key = 'embed_provider_host'", want.Host); err != nil {
			return fmt.Errorf("restamp provider host: %w", err)
		}
	}
	return nil
}

func (s *Store) initSpace(sp EmbeddingSpace) error {
	if sp.Dims <= 0 {
		return fmt.Errorf("invalid embedding dims %d", sp.Dims)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin space init: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(fmt.Sprintf(
		"CREATE VIRTUAL TABLE chunks_vec USING vec0(embedding float[%d] distance_metric=cosine)", sp.Dims)); err != nil {
		return fmt.Errorf("create vector table: %w", err)
	}
	for key, value := range map[string]string{
		"embed_model":         sp.Model,
		"embed_dims":          strconv.Itoa(sp.Dims),
		"embed_provider_host": sp.Host,
	} {
		if _, err := tx.Exec("INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, value); err != nil {
			return fmt.Errorf("stamp %s: %w", key, err)
		}
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", vecSchemaVersion)); err != nil {
		return fmt.Errorf("bump schema version: %w", err)
	}
	return tx.Commit()
}

func (s *Store) MetaString(key string) (string, error) {
	var v string
	err := s.db.QueryRow("SELECT value FROM meta WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read meta %s: %w", key, err)
	}
	return v, nil
}

func (s *Store) SetMetaString(key, value string) error {
	if !s.writable {
		return errors.New("read-only store")
	}
	_, err := s.db.Exec("INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, value)
	if err != nil {
		return fmt.Errorf("write meta %s: %w", key, err)
	}
	return nil
}

type Stats struct {
	Files  int
	Chunks int
	Space  *EmbeddingSpace
}

func (s *Store) Stats() (Stats, error) {
	var st Stats
	if err := s.db.QueryRow("SELECT count(*) FROM files").Scan(&st.Files); err != nil {
		return st, fmt.Errorf("count files: %w", err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM chunks").Scan(&st.Chunks); err != nil {
		return st, fmt.Errorf("count chunks: %w", err)
	}
	sp, err := s.Space()
	if err == nil {
		st.Space = &sp
	} else if !errors.Is(err, ErrNoEmbeddings) {
		return st, err
	}
	return st, nil
}
