package awfllm

import (
	"testing"

	"github.com/valbaudo/awf/agent"
)

// TestForwardableMatrix asserts the (provider × MIME) accept/reject matrix that
// the three builders consult. Reject covers both a supported-MIME-the-transport-
// cannot-encode case (ollama + pdf) and an entirely-unsupported MIME (any +
// application/zip).
func TestForwardableMatrix(t *testing.T) {
	cases := []struct {
		provider string
		mime     string
		wantMod  modality
		wantOK   bool
	}{
		{providerOpenAI, "application/pdf", modalityDocument, true},
		{providerOpenAI, "image/png", modalityImage, true},
		{providerGemini, "image/png", modalityImage, true},
		{providerGemini, "application/pdf", modalityDocument, true},
		{providerOllama, "image/png", modalityImage, true},
		{providerOllama, "application/pdf", 0, false}, // images only — no document part
		{providerOpenAI, "application/zip", 0, false}, // outside the supported floor
		{providerGemini, "application/zip", 0, false},
		{providerOllama, "application/zip", 0, false},
	}
	for _, tc := range cases {
		m, ok := forwardable(tc.provider, tc.mime)
		if ok != tc.wantOK || (ok && m != tc.wantMod) {
			t.Errorf("forwardable(%q, %q) = (%v, %v), want (%v, %v)",
				tc.provider, tc.mime, m, ok, tc.wantMod, tc.wantOK)
		}
	}
}

// TestSupportedFloorIsClassified is the anti-drift guard: every MIME in the
// agent-package detection floor MUST classify via mimeModality. If someone adds a
// MIME to supportedMIME without giving it an encoding modality here, this fails —
// forcing the conscious accept/reject decision the finding wanted.
func TestSupportedFloorIsClassified(t *testing.T) {
	for _, mime := range agent.SupportedMIMEs() {
		if _, ok := mimeModality(mime); !ok {
			t.Errorf("supported MIME %q from agent.SupportedMIMEs() is not classified by mimeModality; "+
				"classify it (image/document modality) so a transport can forward it", mime)
		}
	}
}

// TestMimeModalityRejectsUnsupported confirms anything outside the floor is
// unclassified (ok=false), so forwardable rejects it before any per-transport switch.
func TestMimeModalityRejectsUnsupported(t *testing.T) {
	for _, mime := range []string{"application/zip", "text/plain", "video/mp4", ""} {
		if _, ok := mimeModality(mime); ok {
			t.Errorf("mimeModality(%q) classified an unsupported MIME", mime)
		}
	}
}
