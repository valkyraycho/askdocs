package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/asg017/sqlite-vec-go-bindings/ncruces"
	_ "github.com/ncruces/go-sqlite3/driver"
)

// The blocking spike from PLAN.md: prove that sqlite-vec's vec0 virtual table
// works through the ncruces WASM driver with CGO_ENABLED=0 on every CI OS
// before anything is built on top of it.
func TestSpikeVec0KNN(t *testing.T) {
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "spike.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	var version string
	if err := db.QueryRow("SELECT vec_version()").Scan(&version); err != nil {
		t.Fatalf("vec_version(): %v — sqlite-vec is not registered", err)
	}
	t.Logf("sqlite-vec %s", version)

	if _, err := db.Exec("CREATE VIRTUAL TABLE v USING vec0(embedding float[4] distance_metric=cosine)"); err != nil {
		t.Fatalf("create vec0 table: %v", err)
	}

	vectors := map[int64][]float32{
		1: {1, 0, 0, 0},
		2: {0.9, 0.1, 0, 0}, // nearest to query
		3: {0, 1, 0, 0},
		4: {0, 0, 1, 0},
	}
	for id, v := range vectors {
		if _, err := db.Exec("INSERT INTO v (rowid, embedding) VALUES (?, ?)", id, serializeF32(v)); err != nil {
			t.Fatalf("insert %d: %v", id, err)
		}
	}

	rows, err := db.Query(
		"SELECT rowid FROM v WHERE embedding MATCH ? ORDER BY distance LIMIT 2",
		serializeF32([]float32{1, 0.05, 0, 0}))
	if err != nil {
		t.Fatalf("knn query: %v", err)
	}
	defer rows.Close()

	var got []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("knn order = %v, want [1 2]", got)
	}
}
