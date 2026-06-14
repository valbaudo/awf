package agent

import (
	"fmt"
	"net/http"
	"strings"
)

// InputFile is one resolved file handed to a containerless adapter. Name is the
// logical label from the step's input_files key (NOT a container path). MIME is
// inferred from the CONTENT by sniffing — never from the file name.
type InputFile struct {
	Name    string
	Content []byte
	MIME    string
}

// supportedMIME is the set a model adapter can forward. Extend additively.
var supportedMIME = map[string]struct{}{
	"application/pdf": {},
	"image/png":       {},
	"image/jpeg":      {},
	"image/webp":      {},
	"image/gif":       {},
}

// DetectMIME infers a forwardable MIME from the CONTENT via http.DetectContentType
// (pure-Go WHATWG sniffing: deterministic across hosts; covers pdf/png/jpeg/gif AND
// webp — the webp signature is in net/http/sniff.go). The file name is deliberately
// NOT consulted: mime.TypeByExtension reads the host MIME database, which varies per
// machine and would make the same workflow non-portable; content is the source of
// truth regardless. Returns an error for anything outside supportedMIME so a bad
// input fails fast (caller -> permanent_failure).
func DetectMIME(name string, content []byte) (string, error) {
	m := http.DetectContentType(content)
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = m[:i] // strip "; charset=..."
	}
	if _, ok := supportedMIME[m]; !ok {
		return "", fmt.Errorf("unsupported input_file content for %q: detected %q (supported: pdf, png, jpeg, webp, gif)", name, m)
	}
	return m, nil
}
