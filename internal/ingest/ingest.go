package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/valkyraycho/askdocs/internal/store"
)

const (
	maxFileBytes     = 1 << 20 // 1MB
	embedBatchSize   = 32
	defaultWorkers   = 4
	corpusRootKey    = "corpus_root"
)

var docExtensions = map[string]bool{
	".md": true, ".markdown": true, ".txt": true, ".rst": true,
}

var skipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "dist": true,
}

type EmbedClient interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	EmbedModel() string
	Host() string
}

type Options struct {
	Root                  string
	DB                    string
	Workers               int
	AllowProviderMismatch bool
	Logf                  func(format string, args ...any)
}

type Stats struct {
	Scanned  int
	Skipped  int
	Embedded int
	Pruned   int
	Failed   int
	Chunks   int
}

type job struct {
	rel      string
	hash     string
	sections []Section
}

type fileResult struct {
	rel    string
	hash   string
	chunks []store.ChunkInput
}

func Run(ctx context.Context, st *store.Store, client EmbedClient, opts Options) (Stats, error) {
	var stats Stats
	if opts.Workers < 1 {
		opts.Workers = defaultWorkers
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}

	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return stats, fmt.Errorf("resolve root: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}

	unlock, err := acquireLock(opts.DB + ".lock")
	if err != nil {
		return stats, err
	}
	defer unlock()

	if err := checkCorpusRoot(st, root); err != nil {
		return stats, err
	}
	if err := preflightSpace(st, client, opts.AllowProviderMismatch); err != nil {
		return stats, err
	}

	manifest, walkErr := walk(root, opts.DB)
	stats.Scanned = len(manifest)

	existing, err := st.FileHashes()
	if err != nil {
		return stats, err
	}

	var jobs []job
	onDisk := make(map[string]bool, len(manifest))
	var readFailures []string
	for _, rel := range manifest {
		onDisk[rel] = true
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			opts.Logf("askdocs: stage=read file=%s error=%v", rel, err)
			readFailures = append(readFailures, rel)
			continue
		}
		sum := sha256.Sum256(data)
		hash := hex.EncodeToString(sum[:])
		if existing[rel] == hash {
			stats.Skipped++
			continue
		}
		sections := ChunkFile(rel, data)
		if len(sections) == 0 {
			stats.Skipped++
			continue
		}
		jobs = append(jobs, job{rel: rel, hash: hash, sections: sections})
		stats.Chunks += len(sections)
	}

	if len(jobs) > 0 {
		opts.Logf("askdocs: embedding %d files (%d chunks) via %s (model %s)",
			len(jobs), stats.Chunks, client.Host(), client.EmbedModel())
		embedded, failed := runPipeline(ctx, st, client, opts, jobs)
		stats.Embedded = embedded
		stats.Failed = failed
	}
	stats.Failed += len(readFailures)

	cleanWalk := walkErr == nil && ctx.Err() == nil && len(readFailures) == 0
	if cleanWalk {
		var stale []string
		for rel := range existing {
			if !onDisk[rel] {
				stale = append(stale, rel)
			}
		}
		if err := st.DeleteFiles(stale); err != nil {
			return stats, err
		}
		stats.Pruned = len(stale)
	} else if walkErr != nil {
		opts.Logf("askdocs: walk incomplete, pruning skipped: %v", walkErr)
	}

	switch {
	case walkErr != nil:
		return stats, fmt.Errorf("scan incomplete: %w", walkErr)
	case stats.Failed > 0:
		return stats, fmt.Errorf("%d file(s) failed — their previous versions are preserved; re-run ingest to retry", stats.Failed)
	case ctx.Err() != nil:
		return stats, ctx.Err()
	}
	return stats, nil
}

func runPipeline(ctx context.Context, st *store.Store, client EmbedClient, opts Options, jobs []job) (embedded, failed int) {
	// a fatal embedding-space error makes every pending job unwritable —
	// cancel so in-flight workers stop spending on embeddings
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobCh := make(chan job)
	resultCh := make(chan fileResult)

	var wg sync.WaitGroup
	var failures sync.Map
	for range opts.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobCh {
				chunks, err := embedJob(ctx, client, j)
				if err != nil {
					opts.Logf("askdocs: stage=embed file=%s error=%v", j.rel, err)
					failures.Store(j.rel, err)
					continue
				}
				resultCh <- fileResult{rel: j.rel, hash: j.hash, chunks: chunks}
			}
		}()
	}
	go func() {
		defer close(jobCh)
		for _, j := range jobs {
			select {
			case jobCh <- j:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	spaceReady := false
	for res := range resultCh {
		if !spaceReady {
			space := store.EmbeddingSpace{
				Model: client.EmbedModel(),
				Dims:  len(res.chunks[0].Vec),
				Host:  client.Host(),
			}
			if err := st.EnsureSpace(space, opts.AllowProviderMismatch); err != nil {
				opts.Logf("askdocs: stage=space error=%v", err)
				failures.Store(res.rel, err)
				cancel()
				continue
			}
			spaceReady = true
		}
		if err := st.ReplaceFile(res.rel, res.hash, res.chunks); err != nil {
			opts.Logf("askdocs: stage=write file=%s error=%v", res.rel, err)
			failures.Store(res.rel, err)
			continue
		}
		embedded++
		opts.Logf("askdocs: ok %s (%d chunks)", res.rel, len(res.chunks))
	}

	failures.Range(func(_, _ any) bool { failed++; return true })
	return embedded, failed
}

func embedJob(ctx context.Context, client EmbedClient, j job) ([]store.ChunkInput, error) {
	chunks := make([]store.ChunkInput, len(j.sections))
	for start := 0; start < len(j.sections); start += embedBatchSize {
		end := min(start+embedBatchSize, len(j.sections))
		texts := make([]string, 0, end-start)
		for _, s := range j.sections[start:end] {
			texts = append(texts, s.Heading+"\n"+s.Content)
		}
		vecs, err := client.Embed(ctx, texts)
		if err != nil {
			return nil, err
		}
		for i, s := range j.sections[start:end] {
			chunks[start+i] = store.ChunkInput{Heading: s.Heading, Content: s.Content, Vec: vecs[i]}
		}
	}
	return chunks, nil
}

func walk(root, dbPath string) ([]string, error) {
	dbBase := filepath.Base(dbPath)
	var manifest []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || skipDirs[name]) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // symlinks could smuggle content from outside the corpus root into the egress
		}
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, dbBase) {
			return nil
		}
		if !docExtensions[strings.ToLower(filepath.Ext(name))] {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxFileBytes {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		manifest = append(manifest, filepath.ToSlash(rel))
		return nil
	})
	return manifest, err
}

func checkCorpusRoot(st *store.Store, root string) error {
	stamped, err := st.MetaString(corpusRootKey)
	if errors.Is(err, store.ErrNotFound) {
		return st.SetMetaString(corpusRootKey, root)
	}
	if err != nil {
		return err
	}
	if stamped != root {
		return fmt.Errorf("this corpus indexes %s — refusing to ingest %s into it (use a separate --db)", stamped, root)
	}
	return nil
}

// preflightSpace fails fast on an embedding-space mismatch before any
// document leaves the machine or API spend happens. Dims are unknown until
// the first response, so only model/host are checked here; the writer
// enforces dims.
func preflightSpace(st *store.Store, client EmbedClient, allowHostChange bool) error {
	got, err := st.Space()
	if errors.Is(err, store.ErrNoEmbeddings) {
		return nil
	}
	if err != nil {
		return err
	}
	return st.EnsureSpace(store.EmbeddingSpace{
		Model: client.EmbedModel(),
		Dims:  got.Dims,
		Host:  client.Host(),
	}, allowHostChange)
}

func acquireLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("another ingest appears to be running (remove %s if stale)", path)
		}
		return nil, fmt.Errorf("acquire ingest lock: %w", err)
	}
	fmt.Fprintf(f, "%d\n", os.Getpid())
	f.Close()
	return func() { os.Remove(path) }, nil
}
