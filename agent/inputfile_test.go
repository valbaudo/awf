package agent

import "testing"

func TestDetectMIME(t *testing.T) {
	pngMagic := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	pdfMagic := []byte("%PDF-1.7\n...")
	cases := []struct {
		name    string
		fname   string
		content []byte
		want    string
		wantErr bool
	}{
		{"pdf by content", "report.pdf", pdfMagic, "application/pdf", false},
		{"png by content", "scan.png", pngMagic, "image/png", false},
		{"png misleading-name (content wins)", "doc.pdf", pngMagic, "image/png", false},
		{"unsupported", "data.bin", []byte{0, 1, 2, 3}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DetectMIME(tc.fname, tc.content)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}
