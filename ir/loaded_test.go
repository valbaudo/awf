package ir

import "testing"

func TestLoadedAssetFileDerivedIntegrity(t *testing.T) {
	f := LoadedAssetFile{Path: "x", Bytes: []byte("abc")}
	if got := f.Size(); got != 3 {
		t.Errorf("Size() = %d, want 3", got)
	}
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" // sha256("abc")
	if got := f.SHA256(); got != want {
		t.Errorf("SHA256() = %q, want %q", got, want)
	}
}
