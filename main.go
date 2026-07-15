package main

import (
	"fmt"
	"os"
)

const usage = `askdocs — RAG over any folder: one binary, one SQLite file

usage:
  askdocs ingest [-db file] <path>        index a folder of docs (.md .txt .rst)
  askdocs search [-db file] [-n 10] <query>   hybrid keyword+semantic search
  askdocs ask    [-db file] <question>    answer with citations, streamed
  askdocs status [-db file]               corpus stats
  askdocs web    [-db file] [-port 4712]  browse + ask at http://127.0.0.1:4712

configuration (env):
  OPENAI_BASE_URL   OpenAI-compatible endpoint (default https://api.openai.com/v1;
                    Ollama: http://localhost:11434/v1)
  OPENAI_API_KEY    API key (optional for loopback endpoints)
  ASKDOCS_EMBED_MODEL / ASKDOCS_CHAT_MODEL / ASKDOCS_DB
`

func main() {
	os.Exit(run(os.Args))
}

func run(args []string) int {
	if len(args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	var err error
	switch cmd := args[1]; cmd {
	case "ingest":
		err = cmdIngest(args[2:])
	case "search":
		err = cmdSearch(args[2:])
	case "ask":
		err = cmdAsk(args[2:])
	case "status":
		err = cmdStatus(args[2:])
	case "web":
		err = cmdWeb(args[2:])
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "askdocs: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "askdocs:", err)
		return 1
	}
	return 0
}
