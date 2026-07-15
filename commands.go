package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/valkyraycho/askdocs/internal/ingest"
	"github.com/valkyraycho/askdocs/internal/llm"
	"github.com/valkyraycho/askdocs/internal/rag"
	"github.com/valkyraycho/askdocs/internal/store"
	"github.com/valkyraycho/askdocs/internal/web"
)

const (
	defaultDBName      = "askdocs.db"
	defaultPort        = 4712
	defaultSearchLimit = 10
)

func cmdIngest(args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ExitOnError)
	dbFlag := fs.String("db", "", "corpus database (default <path>/askdocs.db)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: askdocs ingest [-db file] <path>")
	}
	root := fs.Arg(0)
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return fmt.Errorf("%s is not a directory", root)
	}

	dbPath := *dbFlag
	if dbPath == "" {
		if env := os.Getenv("ASKDOCS_DB"); env != "" {
			dbPath = env
		} else {
			dbPath = filepath.Join(root, defaultDBName)
		}
	}

	cfg, err := llm.ConfigFromEnv()
	if err != nil {
		return err
	}
	st, err := store.OpenIngest(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	stats, err := ingest.Run(context.Background(), st, llm.New(cfg), ingest.Options{
		Root:                  root,
		DB:                    dbPath,
		AllowProviderMismatch: os.Getenv("ASKDOCS_ALLOW_PROVIDER_MISMATCH") != "",
		Logf: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		},
	})
	fmt.Printf("scanned %d · embedded %d · skipped %d (unchanged) · pruned %d · failed %d\n",
		stats.Scanned, stats.Embedded, stats.Skipped, stats.Pruned, stats.Failed)
	return err
}

func cmdSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	dbFlag := fs.String("db", "", "corpus database")
	limit := fs.Int("n", defaultSearchLimit, "max results")
	if err := fs.Parse(args); err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return errors.New("usage: askdocs search [-db file] [-n 10] <query>")
	}
	st, err := store.OpenReadOnly(dbPath(*dbFlag))
	if err != nil {
		return err
	}
	defer st.Close()

	hits, label, err := searchBest(st, query, *limit)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		fmt.Println("no matches")
		return nil
	}
	fmt.Printf("— %s results —\n", label)
	for _, h := range hits {
		fmt.Printf("#%-5d %s\n", h.ID, h.Heading)
	}
	return nil
}

func searchBest(st *store.Store, query string, limit int) ([]store.Hit, string, error) {
	cfg, cfgErr := llm.ConfigFromEnv()
	if cfgErr == nil {
		vecs, err := llm.New(cfg).Embed(context.Background(), []string{query})
		if err == nil {
			hits, err := st.SearchHybrid(query, vecs[0], limit)
			if err == nil {
				return hits, "hybrid", nil
			}
			if !errors.Is(err, store.ErrNoEmbeddings) {
				return nil, "", err
			}
		} else {
			fmt.Fprintf(os.Stderr, "askdocs: embedding unavailable (%v) — keyword search only\n", err)
		}
	} else {
		fmt.Fprintf(os.Stderr, "askdocs: no provider configured (%v) — keyword search only\n", cfgErr)
	}
	hits, err := st.SearchFTS(query, limit)
	return hits, "keyword", err
}

func cmdAsk(args []string) error {
	fs := flag.NewFlagSet("ask", flag.ExitOnError)
	dbFlag := fs.String("db", "", "corpus database")
	if err := fs.Parse(args); err != nil {
		return err
	}
	question := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if question == "" {
		return errors.New("usage: askdocs ask [-db file] <question>")
	}
	cfg, err := llm.ConfigFromEnv()
	if err != nil {
		return err
	}
	st, err := store.OpenReadOnly(dbPath(*dbFlag))
	if err != nil {
		return err
	}
	defer st.Close()

	client := llm.New(cfg)
	ctx := context.Background()
	vecs, err := client.Embed(ctx, []string{question})
	if err != nil {
		return err
	}
	hits, err := st.SearchHybrid(question, vecs[0], rag.TopChunks)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		return errors.New("nothing relevant found in the corpus")
	}

	system, user := rag.BuildPrompt(question, hits)
	if os.Getenv("ASKDOCS_NO_STREAM") != "" {
		answer, err := client.Chat(ctx, system, user)
		if err != nil {
			return err
		}
		fmt.Print(answer)
	} else if err := client.ChatStream(ctx, system, user, func(delta string) error {
		_, err := fmt.Print(delta)
		return err
	}); err != nil {
		return err
	}
	fmt.Println("\n\nsources:")
	for i, h := range hits {
		fmt.Printf("  [%d] %s\n", i+1, h.Heading)
	}
	return nil
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	dbFlag := fs.String("db", "", "corpus database")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := dbPath(*dbFlag)
	st, err := store.OpenReadOnly(path)
	if err != nil {
		return err
	}
	defer st.Close()

	stats, err := st.Stats()
	if err != nil {
		return err
	}
	fmt.Printf("corpus:  %s\n", path)
	if root, err := st.MetaString("corpus_root"); err == nil {
		fmt.Printf("root:    %s\n", root)
	}
	fmt.Printf("files:   %d\nchunks:  %d\n", stats.Files, stats.Chunks)
	if stats.Space != nil {
		fmt.Printf("space:   %s (%d dims) via %s\n", stats.Space.Model, stats.Space.Dims, stats.Space.Host)
	} else {
		fmt.Println("space:   no embeddings yet")
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if fi, err := os.Stat(path + suffix); err == nil {
			fmt.Printf("size:    %s%s %d bytes\n", filepath.Base(path), suffix, fi.Size())
		}
	}
	return nil
}

func cmdWeb(args []string) error {
	fs := flag.NewFlagSet("web", flag.ExitOnError)
	dbFlag := fs.String("db", "", "corpus database")
	port := fs.Int("port", defaultPort, "port to serve on (127.0.0.1 only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := llm.ConfigFromEnv()
	if err != nil {
		return err
	}
	path := dbPath(*dbFlag)
	st, err := store.OpenReadOnly(path)
	if err != nil {
		return err
	}
	defer st.Close()

	name := filepath.Base(path)
	if root, err := st.MetaString("corpus_root"); err == nil {
		name = filepath.Base(root)
	}
	return web.Serve(st, llm.New(cfg), *port, name)
}

func dbPath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if env := os.Getenv("ASKDOCS_DB"); env != "" {
		return env
	}
	return defaultDBName
}
