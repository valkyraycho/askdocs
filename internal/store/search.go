package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	maxQueryRunes = 256
	maxLimit      = 50
	rrfK          = 60
	poolCap       = 100
)

type Hit struct {
	Chunk
	Score float64
}

func clampLimit(limit int) int {
	if limit < 1 {
		return 1
	}
	if limit > maxLimit {
		return maxLimit
	}
	return limit
}

func (s *Store) SearchFTS(query string, limit int) ([]Hit, error) {
	limit = clampLimit(limit)
	match := ftsQuery(query)
	if match == "" {
		return nil, nil
	}
	rows, err := s.db.Query(
		`SELECT rowid, bm25(chunks_fts) FROM chunks_fts WHERE chunks_fts MATCH ? ORDER BY bm25(chunks_fts), rowid LIMIT ?`,
		match, limit)
	if err != nil {
		return nil, fmt.Errorf("fts search: %w", err)
	}
	ranked, err := scanRanked(rows)
	if err != nil {
		return nil, err
	}
	return s.hydrate(ranked)
}

func (s *Store) SearchVec(vec []float32, limit int) ([]Hit, error) {
	limit = clampLimit(limit)
	if _, err := s.Space(); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(
		`SELECT rowid, distance FROM chunks_vec WHERE embedding MATCH ? ORDER BY distance LIMIT ?`,
		serializeF32(vec), limit)
	if err != nil {
		return nil, fmt.Errorf("vector search: %w", err)
	}
	ranked, err := scanRanked(rows)
	if err != nil {
		return nil, err
	}
	return s.hydrate(ranked)
}

// SearchHybrid fuses FTS and vector rankings with Reciprocal Rank Fusion:
// score = Σ 1/(60+rank). Ranks fuse cleanly where bm25 scores and cosine
// distances are incomparable.
func (s *Store) SearchHybrid(query string, vec []float32, limit int) ([]Hit, error) {
	limit = clampLimit(limit)
	pool := limit * 4
	if pool < 20 {
		pool = 20
	}
	if pool > poolCap {
		pool = poolCap
	}

	ftsHits, err := s.SearchFTS(query, pool)
	if err != nil {
		return nil, err
	}
	vecHits, err := s.SearchVec(vec, pool)
	if err != nil {
		return nil, err
	}

	scores := make(map[int64]float64)
	order := []int64{}
	for _, list := range [][]Hit{ftsHits, vecHits} {
		for rank, h := range list {
			if _, seen := scores[h.ID]; !seen {
				order = append(order, h.ID)
			}
			scores[h.ID] += 1.0 / float64(rrfK+rank+1)
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		si, sj := scores[order[i]], scores[order[j]]
		if si != sj {
			return si > sj
		}
		return order[i] < order[j]
	})
	if len(order) > limit {
		order = order[:limit]
	}

	byID := make(map[int64]Chunk, len(ftsHits)+len(vecHits))
	for _, h := range ftsHits {
		byID[h.ID] = h.Chunk
	}
	for _, h := range vecHits {
		byID[h.ID] = h.Chunk
	}
	hits := make([]Hit, 0, len(order))
	for _, id := range order {
		hits = append(hits, Hit{Chunk: byID[id], Score: scores[id]})
	}
	return hits, nil
}

func (s *Store) GetChunk(id int64) (Chunk, error) {
	var c Chunk
	err := s.db.QueryRow("SELECT id, path, heading, content, pos FROM chunks WHERE id = ?", id).
		Scan(&c.ID, &c.Path, &c.Heading, &c.Content, &c.Pos)
	if errors.Is(err, sql.ErrNoRows) {
		return Chunk{}, ErrNotFound
	}
	if err != nil {
		return Chunk{}, fmt.Errorf("get chunk %d: %w", id, err)
	}
	return c, nil
}

func (s *Store) FileChunks(path string) ([]Chunk, error) {
	rows, err := s.db.Query("SELECT id, path, heading, content, pos FROM chunks WHERE path = ? ORDER BY pos", path)
	if err != nil {
		return nil, fmt.Errorf("chunks of %s: %w", path, err)
	}
	defer rows.Close()

	var chunks []Chunk
	for rows.Next() {
		var c Chunk
		if err := rows.Scan(&c.ID, &c.Path, &c.Heading, &c.Content, &c.Pos); err != nil {
			return nil, fmt.Errorf("scan chunk of %s: %w", path, err)
		}
		chunks = append(chunks, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chunk rows of %s: %w", path, err)
	}
	return chunks, nil
}

type rankedRow struct {
	id    int64
	score float64
}

func scanRanked(rows *sql.Rows) ([]rankedRow, error) {
	defer rows.Close()
	var ranked []rankedRow
	for rows.Next() {
		var r rankedRow
		if err := rows.Scan(&r.id, &r.score); err != nil {
			return nil, fmt.Errorf("scan ranked row: %w", err)
		}
		ranked = append(ranked, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ranked rows: %w", err)
	}
	return ranked, nil
}

func (s *Store) hydrate(ranked []rankedRow) ([]Hit, error) {
	hits := make([]Hit, 0, len(ranked))
	for _, r := range ranked {
		c, err := s.GetChunk(r.id)
		if errors.Is(err, ErrNotFound) {
			continue // index/vec row without a chunk should not exist, but must not break search
		}
		if err != nil {
			return nil, err
		}
		hits = append(hits, Hit{Chunk: c, Score: r.score})
	}
	return hits, nil
}

func ftsQuery(query string) string {
	if runes := []rune(query); len(runes) > maxQueryRunes {
		query = string(runes[:maxQueryRunes])
	}
	fields := strings.Fields(query)
	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = `"` + strings.ReplaceAll(f, `"`, `""`) + `"`
	}
	if len(parts) > 0 {
		parts[len(parts)-1] += "*"
	}
	return strings.Join(parts, " ")
}
