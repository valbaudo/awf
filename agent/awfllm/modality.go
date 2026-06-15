package awfllm

import (
	"strings"

	"github.com/valbaudo/awf/agent"
)

// This file centralizes "which input MIME can each transport forward" — previously
// re-decided independently in three per-transport builders (buildOpenAIParts,
// streamOllama, callGemini). The CAPABILITY (accept/reject) lives here as data; the
// ENCODING (genuinely distinct wire formats per transport) stays in each builder.
//
// Consulted at DISPATCH (runtime), NOT by static validation: ir/validate cannot read
// with:.provider (the with:-is-opaque invariant) and the input MIME is content-sniffed
// at runtime (unknowable at validate time), so AWF2003 stays a runtime warning.

// modality is how a transport must ENCODE a supported input MIME.
type modality int

const (
	modalityImage modality = iota
	modalityDocument
)

// mimeModality classifies a (supported) MIME into its encoding modality.
// Returns ok=false for anything outside the supported floor.
func mimeModality(mime string) (modality, bool) {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return modalityImage, true
	case mime == "application/pdf":
		return modalityDocument, true
	default:
		return 0, false
	}
}

// providerForwards declares which modalities each transport can forward. This is
// the single source the three builders consult; adding a MIME (→ a modality) or a
// transport forces a conscious accept/reject decision HERE, not silently in 3 switches.
var providerForwards = map[string]map[modality]bool{
	providerOpenAI:    {modalityImage: true, modalityDocument: true},
	providerGemini:    {modalityImage: true, modalityDocument: true},
	providerOllama:    {modalityImage: true}, // images only — Ollama has no document part
	providerAnthropic: {modalityImage: true, modalityDocument: true},
}

// forwardable returns (modality, true) iff `provider` can forward `mime`.
func forwardable(provider, mime string) (modality, bool) {
	m, ok := mimeModality(mime)
	if !ok {
		return 0, false
	}
	return m, providerForwards[provider][m]
}

// unsupportedMIMEErr is the shared rejection (consistent message across transports).
func unsupportedMIMEErr(mime, hint string) error {
	return &agent.ErrInvalidConfig{Ref: AdapterRef, Key: "input_files", Reason: "this transport cannot forward MIME " + mime + hint}
}
