// Command genrates regenerates pricing/rates.json from the models.dev pricing
// database (with a LiteLLM fallback), filtered to a curated allowlist of the
// models AWF prices. It is a BUILD/MAINTENANCE tool — run via `make pricing-regen`
// or the scheduled pricing CI, NEVER by the awf binary. It uses only the standard
// library plus the in-repo pricing package, so it adds no dependency to the awf
// binary's module graph, and it touches the network ONLY here (at regen time) —
// `go build` and `awf run` stay fully offline and deterministic; go:embed sees
// the committed, human-reviewed rates.json.
//
// Freshness is automated by a weekly GitHub Actions job that runs this tool and
// opens a review PR (.github/workflows/pricing.yml). The committed table stays a
// CURATED SUBSET (see allowlist.txt) — the whole upstream is NOT embedded.
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	_ "embed"
)

const (
	modelsDevURL = "https://models.dev/api.json"
	liteLLMURL   = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
)

// firstParty is the ordered set of models.dev provider sections we trust for a
// price, searched in priority order. Restricting to first-party vendors avoids a
// reseller/aggregator section carrying a marked-up rate for the same model id.
var firstParty = []string{"openai", "anthropic", "google", "deepseek", "xai", "mistral", "cohere"}

//go:embed allowlist.txt
var allowlistRaw string

// parseAllowlist returns the model ids in allowlist.txt, skipping blank lines and
// `#` comments. Order is preserved (informational only — render sorts the output).
func parseAllowlist(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

func fetch(url string) ([]byte, error) {
	c := &http.Client{Timeout: 60 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func main() {
	out := flag.String("out", "pricing/rates.json", "destination path for the generated rates.json")
	flag.Parse()

	syncDate := time.Now().UTC().Format("2006-01-02")
	allowlist := parseAllowlist(allowlistRaw)
	if len(allowlist) == 0 {
		fmt.Fprintln(os.Stderr, "genrates: allowlist.txt is empty")
		os.Exit(1)
	}

	modelsDevRaw, err := fetch(modelsDevURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "genrates: fetch models.dev: %v\n", err)
		os.Exit(1)
	}

	table, err := build(modelsDevRaw, allowlist, firstParty, syncDate, func() ([]byte, error) {
		return fetch(liteLLMURL)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "genrates: %v\n", err)
		os.Exit(1)
	}

	data, err := render(table)
	if err != nil {
		fmt.Fprintf(os.Stderr, "genrates: render: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "genrates: write %s: %v\n", *out, err)
		os.Exit(1)
	}
	fmt.Printf("genrates: wrote %d models to %s\n", len(table), *out)
}
