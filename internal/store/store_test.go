package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

const testDims = 4

func testSpace() EmbeddingSpace {
	return EmbeddingSpace{Model: "fake-embed", Dims: testDims, Host: "fake.test"}
}

func newIngestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "askdocs.db")
	s, err := OpenIngest(path)
	if err != nil {
		t.Fatalf("OpenIngest: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func newEmbeddedStore(t *testing.T) (*Store, string) {
	t.Helper()
	s, path := newIngestStore(t)
	if err := s.EnsureSpace(testSpace(), false); err != nil {
		t.Fatalf("EnsureSpace: %v", err)
	}
	return s, path
}

func vec(a, b, c, d float32) []float32 { return []float32{a, b, c, d} }

func mustReplace(t *testing.T, s *Store, path, hash string, chunks ...ChunkInput) {
	t.Helper()
	if err := s.ReplaceFile(path, hash, chunks); err != nil {
		t.Fatalf("ReplaceFile(%s): %v", path, err)
	}
}

func TestOpenIngestCreatesStampedCorpus(t *testing.T) {
	s, path := newIngestStore(t)

	var appID int64
	if err := s.db.QueryRow("PRAGMA application_id").Scan(&appID); err != nil {
		t.Fatalf("application_id: %v", err)
	}
	if appID != applicationID {
		t.Errorf("application_id = %#x, want %#x", appID, applicationID)
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if version != baseSchemaVersion {
		t.Errorf("user_version = %d, want %d", version, baseSchemaVersion)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("db perm = %o, want 600", fi.Mode().Perm())
		}
	}
}

func TestOpenIngestIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "askdocs.db")
	s1, err := OpenIngest(path)
	if err != nil {
		t.Fatalf("first OpenIngest: %v", err)
	}
	mustReplace(t, s1, "a.md", "h1", ChunkInput{Heading: "a.md", Content: "hello world"})
	s1.Close()

	s2, err := OpenIngest(path)
	if err != nil {
		t.Fatalf("second OpenIngest: %v", err)
	}
	defer s2.Close()
	hits, err := s2.SearchFTS("hello", 10)
	if err != nil || len(hits) != 1 {
		t.Errorf("data lost across reopen: %v %v", hits, err)
	}
}

func TestOpenIngestRefusesForeignFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foreign.db")
	if err := os.WriteFile(path, []byte("definitely not a sqlite database, definitely long enough"), 0o600); err != nil {
		t.Fatalf("write foreign file: %v", err)
	}
	if _, err := OpenIngest(path); err == nil {
		t.Fatalf("OpenIngest over garbage file succeeded, want error")
	}
}

func TestOpenReadOnly(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		if _, err := OpenReadOnly(filepath.Join(t.TempDir(), "nope.db")); err == nil {
			t.Fatalf("OpenReadOnly on missing path succeeded, want error")
		}
	})
	t.Run("rejects foreign corpus", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "other.db")
		s, err := OpenIngest(path)
		if err != nil {
			t.Fatalf("OpenIngest: %v", err)
		}
		if _, err := s.db.Exec(fmt.Sprintf("PRAGMA application_id = %d", 0x12345678)); err != nil {
			t.Fatalf("restamp: %v", err)
		}
		s.Close()
		if _, err := OpenReadOnly(path); err == nil {
			t.Fatalf("OpenReadOnly accepted foreign application_id, want error")
		}
	})
	t.Run("cannot write", func(t *testing.T) {
		s, path := newIngestStore(t)
		mustReplace(t, s, "a.md", "h", ChunkInput{Heading: "a.md", Content: "content"})
		s.Close()

		ro, err := OpenReadOnly(path)
		if err != nil {
			t.Fatalf("OpenReadOnly: %v", err)
		}
		defer ro.Close()
		if err := ro.ReplaceFile("b.md", "h2", []ChunkInput{{Heading: "b", Content: "x"}}); err == nil {
			t.Errorf("write through read-only store succeeded, want error")
		}
		if _, err := ro.db.Exec("INSERT INTO meta (key, value) VALUES ('x', 'y')"); err == nil {
			t.Errorf("raw write on read-only connection succeeded, want error")
		}
	})
}

func TestEnsureSpace(t *testing.T) {
	s, _ := newIngestStore(t)

	if _, err := s.Space(); !errors.Is(err, ErrNoEmbeddings) {
		t.Fatalf("Space before init = %v, want ErrNoEmbeddings", err)
	}
	if err := s.EnsureSpace(testSpace(), false); err != nil {
		t.Fatalf("first EnsureSpace: %v", err)
	}
	got, err := s.Space()
	if err != nil || got != testSpace() {
		t.Fatalf("Space = %+v, %v", got, err)
	}

	if err := s.EnsureSpace(testSpace(), false); err != nil {
		t.Errorf("same-space EnsureSpace: %v", err)
	}
	if err := s.EnsureSpace(EmbeddingSpace{Model: "other-model", Dims: testDims, Host: "fake.test"}, false); err == nil {
		t.Errorf("model mismatch accepted, want error")
	}
	if err := s.EnsureSpace(EmbeddingSpace{Model: "fake-embed", Dims: 8, Host: "fake.test"}, false); err == nil {
		t.Errorf("dims mismatch accepted, want error")
	}
	if err := s.EnsureSpace(EmbeddingSpace{Model: "fake-embed", Dims: testDims, Host: "elsewhere.test"}, false); err == nil {
		t.Errorf("host mismatch without override accepted, want error")
	}
	if err := s.EnsureSpace(EmbeddingSpace{Model: "fake-embed", Dims: testDims, Host: "elsewhere.test"}, true); err != nil {
		t.Errorf("host override rejected: %v", err)
	}
	got, _ = s.Space()
	if got.Host != "elsewhere.test" {
		t.Errorf("host not restamped: %+v", got)
	}
	if err := s.EnsureSpace(EmbeddingSpace{Model: "other", Dims: 8, Host: "elsewhere.test"}, true); err == nil {
		t.Errorf("model/dims mismatch must have no override, got success")
	}
}

func TestReplaceFileRoundtripAndReplace(t *testing.T) {
	s, _ := newEmbeddedStore(t)

	mustReplace(t, s, "docs/a.md", "hash1",
		ChunkInput{Heading: "a.md › Setup", Content: "install the docker daemon", Vec: vec(1, 0, 0, 0)},
		ChunkInput{Heading: "a.md › Usage", Content: "run the kubernetes cluster", Vec: vec(0, 1, 0, 0)},
	)

	hashes, err := s.FileHashes()
	if err != nil || hashes["docs/a.md"] != "hash1" {
		t.Fatalf("FileHashes = %v, %v", hashes, err)
	}
	if hits, _ := s.SearchFTS("docker", 10); len(hits) != 1 || !strings.Contains(hits[0].Content, "docker") {
		t.Errorf("FTS after insert: %v", hits)
	}
	if hits, _ := s.SearchVec(vec(0.9, 0.1, 0, 0), 1); len(hits) != 1 || hits[0].Heading != "a.md › Setup" {
		t.Errorf("vec KNN after insert: %+v", hits)
	}

	mustReplace(t, s, "docs/a.md", "hash2",
		ChunkInput{Heading: "a.md › New", Content: "everything is podman now", Vec: vec(0, 0, 1, 0)},
	)
	if hits, _ := s.SearchFTS("docker", 10); len(hits) != 0 {
		t.Errorf("old FTS rows survived replace: %v", hits)
	}
	if hits, _ := s.SearchVec(vec(1, 0, 0, 0), 10); len(hits) != 1 || hits[0].Heading != "a.md › New" {
		t.Errorf("old vec rows survived replace: %+v", hits)
	}
	chunks, err := s.FileChunks("docs/a.md")
	if err != nil || len(chunks) != 1 || chunks[0].Content != "everything is podman now" {
		t.Errorf("FileChunks after replace: %v, %v", chunks, err)
	}
}

func TestDeleteFilesPrunes(t *testing.T) {
	s, _ := newEmbeddedStore(t)
	mustReplace(t, s, "a.md", "h1", ChunkInput{Heading: "a", Content: "alpha text", Vec: vec(1, 0, 0, 0)})
	mustReplace(t, s, "b.md", "h2", ChunkInput{Heading: "b", Content: "beta text", Vec: vec(0, 1, 0, 0)})

	if err := s.DeleteFiles([]string{"a.md"}); err != nil {
		t.Fatalf("DeleteFiles: %v", err)
	}
	hashes, _ := s.FileHashes()
	if len(hashes) != 1 || hashes["b.md"] == "" {
		t.Errorf("hashes after prune = %v", hashes)
	}
	if hits, _ := s.SearchFTS("alpha", 10); len(hits) != 0 {
		t.Errorf("pruned file still in FTS")
	}
	if hits, _ := s.SearchVec(vec(1, 0, 0, 0), 10); len(hits) != 1 || hits[0].Path != "b.md" {
		t.Errorf("pruned file still in vec index: %+v", hits)
	}
}

func TestSearchFTSHostileInputs(t *testing.T) {
	s, _ := newIngestStore(t)
	mustReplace(t, s, "a.md", "h", ChunkInput{Heading: "a", Content: "some sqlite note"})

	hostile := []string{
		`"`, `""`, `AND`, `OR`, `NOT`, `-`, `(`, `(((`, `*`, `"unclosed`,
		`heading:x`, `^first`, strings.Repeat("x", 10*1024),
	}
	for _, q := range hostile {
		if _, err := s.SearchFTS(q, 10); err != nil {
			t.Errorf("SearchFTS(%.20q): %v", q, err)
		}
	}
	if hits, err := s.SearchFTS("   ", 10); err != nil || hits != nil {
		t.Errorf("blank query: %v, %v", hits, err)
	}
}

func TestSearchVecRequiresEmbeddings(t *testing.T) {
	s, _ := newIngestStore(t)
	if _, err := s.SearchVec(vec(1, 0, 0, 0), 5); !errors.Is(err, ErrNoEmbeddings) {
		t.Errorf("SearchVec without phase 2 = %v, want ErrNoEmbeddings", err)
	}
	if _, err := s.SearchHybrid("q", vec(1, 0, 0, 0), 5); !errors.Is(err, ErrNoEmbeddings) {
		t.Errorf("SearchHybrid without phase 2 = %v, want ErrNoEmbeddings", err)
	}
}

func TestSearchHybridRRF(t *testing.T) {
	s, _ := newEmbeddedStore(t)
	// A: FTS rank 1 but vector rank 3. C: FTS rank 2 and vector rank 1 —
	// RRF (1/62 + 1/61) must fuse C above A (1/61 + 1/63).
	mustReplace(t, s, "a.md", "ha", ChunkInput{Heading: "a", Content: "grpc timeout retry policy grpc grpc", Vec: vec(0, 0, 1, 0)})
	mustReplace(t, s, "b.md", "hb", ChunkInput{Heading: "b", Content: "network call deadline handling", Vec: vec(0.9, 0.1, 0, 0)})
	mustReplace(t, s, "c.md", "hc", ChunkInput{Heading: "c", Content: "grpc deadline configuration", Vec: vec(1, 0, 0, 0)})

	hits, err := s.SearchHybrid("grpc", vec(1, 0, 0, 0), 3)
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("got %d hits, want 3", len(hits))
	}
	if hits[0].Path != "c.md" {
		t.Errorf("RRF top = %s, want c.md (ranked on both lists); order: %v", hits[0].Path, paths(hits))
	}
	for i := 1; i < len(hits); i++ {
		if hits[i].Score > hits[i-1].Score {
			t.Errorf("hits not sorted by fused score: %v", paths(hits))
		}
	}
}

func TestGetChunkAndNotFound(t *testing.T) {
	s, _ := newEmbeddedStore(t)
	mustReplace(t, s, "a.md", "h", ChunkInput{Heading: "a", Content: "target chunk", Vec: vec(1, 0, 0, 0)})
	chunks, _ := s.FileChunks("a.md")
	if len(chunks) != 1 {
		t.Fatalf("FileChunks = %v", chunks)
	}
	got, err := s.GetChunk(chunks[0].ID)
	if err != nil || got.Content != "target chunk" {
		t.Errorf("GetChunk = %+v, %v", got, err)
	}
	if _, err := s.GetChunk(9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetChunk(9999) = %v, want ErrNotFound", err)
	}
}

func TestConcurrentFirstOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "askdocs.db")
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := OpenIngest(path)
			if err != nil {
				errs[i] = err
				return
			}
			errs[i] = s.ReplaceFile(fmt.Sprintf("f%d.md", i), "h", []ChunkInput{{Heading: "h", Content: fmt.Sprintf("race %d", i)}})
			s.Close()
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}
}

func TestCheckpointOnWritableClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "askdocs.db")
	s, err := OpenIngest(path)
	if err != nil {
		t.Fatalf("OpenIngest: %v", err)
	}
	mustReplace(t, s, "a.md", "h", ChunkInput{Heading: "a", Content: "wal content"})
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() > 0 {
		t.Errorf("WAL sidecar not truncated on clean writable close (%d bytes)", fi.Size())
	}
}

func TestStats(t *testing.T) {
	s, _ := newEmbeddedStore(t)
	mustReplace(t, s, "a.md", "h1",
		ChunkInput{Heading: "a1", Content: "one", Vec: vec(1, 0, 0, 0)},
		ChunkInput{Heading: "a2", Content: "two", Vec: vec(0, 1, 0, 0)})
	mustReplace(t, s, "b.md", "h2", ChunkInput{Heading: "b", Content: "three", Vec: vec(0, 0, 1, 0)})

	st, err := s.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.Files != 2 || st.Chunks != 3 || st.Space == nil || st.Space.Model != "fake-embed" {
		t.Errorf("Stats = %+v", st)
	}
}

func paths(hits []Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Path
	}
	return out
}
