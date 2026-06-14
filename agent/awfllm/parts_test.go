package awfllm

import (
	"encoding/base64"
	"testing"

	"github.com/valbaudo/awf/agent"
)

func TestBuildOpenAIParts_PDFAndImage(t *testing.T) {
	files := []agent.InputFile{
		{Name: "doc", MIME: "application/pdf", Content: []byte("%PDF-1.7")},
		{Name: "scan", MIME: "image/png", Content: []byte{0x89, 'P', 'N', 'G'}},
	}
	parts, err := buildOpenAIParts("extract", files)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 3 { // file + image + text (documents FIRST, prompt LAST — prefix caching)
		t.Fatalf("parts=%d", len(parts))
	}
	// Document parts come FIRST so the stable document is the cacheable common
	// prefix and the varying prompt is the suffix (see buildOpenAIParts).
	if parts[0].OfFile == nil || parts[0].OfFile.File.FileData.Value != "data:application/pdf;base64,"+base64.StdEncoding.EncodeToString([]byte("%PDF-1.7")) {
		t.Fatalf("pdf file part must be FIRST: %+v", parts[0].OfFile)
	}
	if parts[1].OfImageURL == nil {
		t.Fatalf("image part must be SECOND")
	}
	if parts[2].OfText == nil || parts[2].OfText.Text != "extract" {
		t.Fatalf("prompt text part must be LAST: %+v", parts[2].OfText)
	}
}
