package ingest

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/valkyraycho/askdocs/internal/store"
)

type fakeEmbedder struct {
	mu      sync.Mutex
	batches [][]string
	failOn  string
	host    string
}

func (f *fakeEmbedder) EmbedModel() string { return "fake-embed" }

func (f *fakeEmbedder) Host() string {
	if f.host != "" {
		return f.host
	}
	return "fake.test"
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.mu.Lock()
	f.batches = append(f.batches, texts)
	f.mu.Unlock()
	out := make([][]float32, len(texts))
	for i, t := range texts {
		if f.failOn != "" && strings.Contains(t, f.failOn) {
			return nil, errors.New("simulated embedding failure")
		}
		h := fnv.New32a()
		h.Write([]byte(t))
		v := h.Sum32()
		out[i] = []float32{
			float32(v & 0xff), float32(v >> 8 & 0xff), float32(v >> 16 & 0xff), float32(v >> 24 & 0xff),
		}
	}
	return out, nil
}

func (f *fakeEmbedder) embeddedTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []string
	for _, b := range f.batches {
		all = append(all, b...)
	}
	return all
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}

func testOpts(root string) Options {
	return Options{
		Root:    root,
		DB:      filepath.Join(root, "askdocs.db"),
		Workers: 2,
		Logf:    func(string, ...any) {},
	}
}

func openStore(t *testing.T, dbPath string) *store.Store {
	t.Helper()
	st, err := store.OpenIngest(dbPath)
	if err != nil {
		t.Fatalf("OpenIngest: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestIngestFreshCorpus(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"guide.md":       "# Guide\n\ndocker networking content\n",
		"notes/plain.txt": "plain text about kubernetes\n",
		".hidden.md":      "should be skipped\n",
		".secret/x.md":    "hidden dir skipped\n",
		"node_modules/dep.md": "vendored skipped\n",
		"binary.png":      "not a doc\n",
	})
	st := openStore(t, filepath.Join(root, "askdocs.db"))
	emb := &fakeEmbedder{}

	stats, err := Run(context.Background(), st, emb, testOpts(root))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Embedded != 2 || stats.Failed != 0 {
		t.Errorf("stats = %+v, want 2 embedded", stats)
	}
	for _, text := range emb.embeddedTexts() {
		if strings.Contains(text, "skipped") {
			t.Errorf("skipped file was embedded: %q", text)
		}
	}
	hits, err := st.SearchHybrid("docker", []float32{1, 0, 0, 0}, 5)
	if err != nil || len(hits) == 0 {
		t.Errorf("hybrid search after ingest: %v, %v", hits, err)
	}
	sp, err := st.Space()
	if err != nil || sp.Model != "fake-embed" || sp.Dims != 4 || sp.Host != "fake.test" {
		t.Errorf("space = %+v, %v", sp, err)
	}
	if root2, err := st.MetaString("corpus_root"); err != nil || root2 == "" {
		t.Errorf("corpus_root not stamped: %q %v", root2, err)
	}
}

func TestIngestIncrementalOnlyChangedFiles(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"a.md": "# A\n\nalpha original\n",
		"b.md": "# B\n\nbeta stays\n",
	})
	st := openStore(t, filepath.Join(root, "askdocs.db"))
	emb := &fakeEmbedder{}
	if _, err := Run(context.Background(), st, emb, testOpts(root)); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	writeTree(t, root, map[string]string{"a.md": "# A\n\nalpha CHANGED\n"})
	emb2 := &fakeEmbedder{}
	stats, err := Run(context.Background(), st, emb2, testOpts(root))
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if stats.Embedded != 1 || stats.Skipped != 1 {
		t.Errorf("stats = %+v, want 1 embedded / 1 skipped", stats)
	}
	for _, text := range emb2.embeddedTexts() {
		if strings.Contains(text, "beta") {
			t.Errorf("unchanged file re-embedded")
		}
	}
	hits, _ := st.SearchFTS("CHANGED", 5)
	if len(hits) != 1 {
		t.Errorf("new content not searchable")
	}
	if hits, _ := st.SearchFTS("original", 5); len(hits) != 0 {
		t.Errorf("old content survived")
	}
}

func TestIngestPrunesDeletedFiles(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"keep.md": "# K\n\nkeep me\n",
		"gone.md": "# G\n\ndelete me\n",
	})
	st := openStore(t, filepath.Join(root, "askdocs.db"))
	if _, err := Run(context.Background(), st, &fakeEmbedder{}, testOpts(root)); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	os.Remove(filepath.Join(root, "gone.md"))
	stats, err := Run(context.Background(), st, &fakeEmbedder{}, testOpts(root))
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if stats.Pruned != 1 {
		t.Errorf("stats = %+v, want 1 pruned", stats)
	}
	if hits, _ := st.SearchFTS("delete", 5); len(hits) != 0 {
		t.Errorf("pruned file still searchable")
	}
}

func TestIngestPartialFailureKeepsOldVersion(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"good.md": "# G\n\ngood original\n",
		"bad.md":  "# B\n\nbad original\n",
	})
	st := openStore(t, filepath.Join(root, "askdocs.db"))
	if _, err := Run(context.Background(), st, &fakeEmbedder{}, testOpts(root)); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	writeTree(t, root, map[string]string{
		"good.md": "# G\n\ngood updated\n",
		"bad.md":  "# B\n\nbad FAILME updated\n",
	})
	stats, err := Run(context.Background(), st, &fakeEmbedder{failOn: "FAILME"}, testOpts(root))
	if err == nil {
		t.Fatalf("Run with failing file succeeded, want error")
	}
	if stats.Failed != 1 || stats.Embedded != 1 {
		t.Errorf("stats = %+v, want 1 failed / 1 embedded", stats)
	}
	if hits, _ := st.SearchFTS("original", 5); len(hits) != 1 {
		t.Errorf("failed file's old version not preserved: %d hits", len(hits))
	}
	if hits, _ := st.SearchFTS("updated", 5); len(hits) != 1 {
		t.Errorf("good file not updated independently: %d hits", len(hits))
	}
}

func TestIngestLockfileConflict(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.md": "# A\n\ntext\n"})
	st := openStore(t, filepath.Join(root, "askdocs.db"))

	lock := filepath.Join(root, "askdocs.db.lock")
	if err := os.WriteFile(lock, []byte("12345"), 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	emb := &fakeEmbedder{}
	if _, err := Run(context.Background(), st, emb, testOpts(root)); err == nil {
		t.Fatalf("Run with existing lock succeeded, want error")
	}
	if len(emb.batches) != 0 {
		t.Errorf("embedding happened despite lock")
	}
}

func TestIngestLockReleasedAfterRun(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.md": "# A\n\ntext\n"})
	st := openStore(t, filepath.Join(root, "askdocs.db"))
	if _, err := Run(context.Background(), st, &fakeEmbedder{}, testOpts(root)); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if _, err := Run(context.Background(), st, &fakeEmbedder{}, testOpts(root)); err != nil {
		t.Errorf("second Run blocked by stale lock: %v", err)
	}
}

func TestIngestRootMismatchRejected(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	writeTree(t, rootA, map[string]string{"a.md": "# A\n\ntext\n"})
	writeTree(t, rootB, map[string]string{"b.md": "# B\n\ntext\n"})
	db := filepath.Join(rootA, "askdocs.db")
	st := openStore(t, db)
	if _, err := Run(context.Background(), st, &fakeEmbedder{}, testOpts(rootA)); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	opts := testOpts(rootB)
	opts.DB = db
	if _, err := Run(context.Background(), st, &fakeEmbedder{}, opts); err == nil {
		t.Fatalf("Run against a different root reused the corpus, want error")
	}
}

func TestIngestPreflightSpaceMismatchBeforeSpend(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.md": "# A\n\ntext\n"})
	st := openStore(t, filepath.Join(root, "askdocs.db"))
	if err := st.EnsureSpace(store.EmbeddingSpace{Model: "other-model", Dims: 4, Host: "fake.test"}, false); err != nil {
		t.Fatalf("seed space: %v", err)
	}

	writeTree(t, root, map[string]string{"a.md": "# A\n\nchanged\n"})
	emb := &fakeEmbedder{}
	if _, err := Run(context.Background(), st, emb, testOpts(root)); err == nil {
		t.Fatalf("Run with mismatched embed model succeeded, want error")
	}
	if len(emb.batches) != 0 {
		t.Errorf("API spend happened despite preflight mismatch: %d batches", len(emb.batches))
	}
}

func TestIngestBatchesCapAt32(t *testing.T) {
	var parts []string
	for i := 0; i < 70; i++ {
		parts = append(parts, fmt.Sprintf("## H%d\n\nsection %d text", i, i))
	}
	root := t.TempDir()
	writeTree(t, root, map[string]string{"big.md": strings.Join(parts, "\n\n")})
	st := openStore(t, filepath.Join(root, "askdocs.db"))
	emb := &fakeEmbedder{}
	if _, err := Run(context.Background(), st, emb, testOpts(root)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(emb.batches) < 3 {
		t.Fatalf("expected >=3 batches for 70 chunks, got %d", len(emb.batches))
	}
	for i, b := range emb.batches {
		if len(b) > 32 {
			t.Errorf("batch %d has %d items, cap is 32", i, len(b))
		}
	}
}

func TestIngestSkipsCorpusDBFiles(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.md": "# A\n\ntext\n"})
	st := openStore(t, filepath.Join(root, "askdocs.db"))
	// decoys that look ingestible but belong to the corpus
	writeTree(t, root, map[string]string{"askdocs.db.lock.txt": "not really\n"})

	if _, err := Run(context.Background(), st, &fakeEmbedder{}, testOpts(root)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	hashes, _ := st.FileHashes()
	for path := range hashes {
		if strings.HasPrefix(path, "askdocs.db") && !strings.HasSuffix(path, ".txt") {
			t.Errorf("corpus artifact ingested: %s", path)
		}
	}
}

func TestIngestWalkErrorSkipsPrune(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based unreadable dir is unix-only")
	}
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"ok.md":         "# OK\n\nvisible\n",
		"locked/in.md":  "# L\n\ninside locked dir\n",
	})
	st := openStore(t, filepath.Join(root, "askdocs.db"))
	if _, err := Run(context.Background(), st, &fakeEmbedder{}, testOpts(root)); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	locked := filepath.Join(root, "locked")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })

	if _, err := Run(context.Background(), st, &fakeEmbedder{}, testOpts(root)); err == nil {
		t.Fatalf("Run over unreadable dir succeeded, want error")
	}
	if hits, _ := st.SearchFTS("inside", 5); len(hits) != 1 {
		t.Errorf("file behind broken walk was pruned — data loss")
	}
}
