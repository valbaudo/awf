package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

func TestStoreRunStartedAssetDerivesIntegrityFromBytes(t *testing.T) {
	blobs, err := state.OpenBlobs(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatalf("OpenBlobs: %v", err)
	}
	body := []byte("integrity-bearing bytes\n")
	out, err := StoreRunStartedAssets(blobs, map[string]ir.LoadedAsset{
		"a": {ID: "a", DeclaredPath: "a.txt", Files: []ir.LoadedAssetFile{{Path: ".", Bytes: body}}},
	})
	if err != nil {
		t.Fatalf("StoreRunStartedAssets: %v", err)
	}
	f := out["a"].Files[0]
	if f.Size != int64(len(body)) {
		t.Errorf("manifest Size = %d, want %d", f.Size, len(body))
	}
	sum := sha256.Sum256(body)
	if want := hex.EncodeToString(sum[:]); f.SHA256 != want {
		t.Errorf("manifest SHA256 = %q, want %q (must hash the persisted bytes)", f.SHA256, want)
	}
}
