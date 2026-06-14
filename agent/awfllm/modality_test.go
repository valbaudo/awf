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

func TestForwardable_Anthropic(t *testing.T) {
	if m, ok := forwardable(providerAnthropic, "image/png"); !ok || m != modalityImage {
		t.Errorf("anthropic image/png: m=%v ok=%v, want image,true", m, ok)
	}
	if m, ok := forwardable(providerAnthropic, "application/pdf"); !ok || m != modalityDocument {
		t.Errorf("anthropic application/pdf: m=%v ok=%v, want document,true", m, ok)
	}
	if _, ok := forwardable(providerAnthropic, "text/plain"); ok {
		t.Error("anthropic text/plain must be rejected")
	}
}
