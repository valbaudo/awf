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
	if len(parts) != 3 { // text + file + image
		t.Fatalf("parts=%d", len(parts))
	}
	if parts[1].OfFile == nil || parts[1].OfFile.File.FileData.Value != "data:application/pdf;base64,"+base64.StdEncoding.EncodeToString([]byte("%PDF-1.7")) {
		t.Fatalf("pdf file part wrong: %+v", parts[1].OfFile)
	}
	if parts[2].OfImageURL == nil {
		t.Fatalf("image part missing")
	}
}
