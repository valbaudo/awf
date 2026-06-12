package pricing

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultLoadsEmbedded(t *testing.T) {
	if len(Default()) == 0 {
		t.Fatal("embedded rates empty")
	}
}

func TestOverrideWholeEntryReplace(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "p.json")
	if err := os.WriteFile(f, []byte(`{"zzz-test-model":{"currency":"USD","input_per_m":99,"output_per_m":1,"cache_read_per_m":0,"cache_write_per_m":0}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWF_PRICING_FILE", f)
	tb, err := loadTable()
	if err != nil {
		t.Fatal(err)
	}
	if tb["zzz-test-model"].InputPerM != 99 {
		t.Fatalf("override not applied: %+v", tb["zzz-test-model"])
	}
}

func TestOverrideRejectsBadRates(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "p.json")
	if err := os.WriteFile(f, []byte(`{"x":{"currency":"USD","input_per_m":-1,"output_per_m":1,"cache_read_per_m":0,"cache_write_per_m":0}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWF_PRICING_FILE", f)
	if _, err := loadTable(); err == nil {
		t.Fatal("negative rate must be rejected")
	}
}
