package loader

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/valbaudo/awf/ir"
)

var errSymlinkComponent = errors.New("symlink path component")

const MaxAssetFileBytes int64 = 64 << 20
const MaxAssetTotalBytes int64 = 256 << 20
const MaxAssetFiles = 10000

func loadAssets(root *os.Root, declared map[string]string) (map[string]ir.LoadedAsset, error) {
	return loadAssetsWithLimits(root, declared, assetLimits{
		fileBytes:  MaxAssetFileBytes,
		totalBytes: MaxAssetTotalBytes,
		files:      MaxAssetFiles,
	})
}

type assetLimits struct {
	fileBytes  int64
	totalBytes int64
	files      int
}

type assetLimitTracker struct {
	limits assetLimits
	bytes  int64
	files  int
}

func (t *assetLimitTracker) add(id, manifestPath string, size int64) error {
	if t.files+1 > t.limits.files {
		return fmt.Errorf("asset %q path %q: file count exceeds limit %d", id, manifestPath, t.limits.files)
	}
	if t.bytes+size > t.limits.totalBytes {
		return fmt.Errorf("asset %q path %q: total bytes exceed limit %d", id, manifestPath, t.limits.totalBytes)
	}
	t.files++
	t.bytes += size
	return nil
}

func loadAssetsWithLimits(root *os.Root, declared map[string]string, limits assetLimits) (map[string]ir.LoadedAsset, error) {
	out := make(map[string]ir.LoadedAsset, len(declared))
	tracker := assetLimitTracker{limits: limits}
	for id, path := range declared {
		asset, err := loadAsset(root, id, path, &tracker)
		if err != nil {
			return nil, err
		}
		out[id] = asset
	}
	return out, nil
}

func loadAsset(root *os.Root, id, declared string, tracker *assetLimitTracker) (ir.LoadedAsset, error) {
	rel, err := assetRelPath(declared)
	if err != nil {
		return ir.LoadedAsset{}, fmt.Errorf("asset %q path %q: %w", id, declared, err)
	}
	if err := rejectSymlinkComponents(root, rel); err != nil {
		return ir.LoadedAsset{}, fmt.Errorf("asset %q path %q: %w", id, declared, err)
	}
	info, err := root.Lstat(rel)
	if err != nil {
		return ir.LoadedAsset{}, fmt.Errorf("asset %q path %q: stat: %w", id, declared, err)
	}
	asset := ir.LoadedAsset{ID: id, DeclaredPath: rel, IsDir: info.IsDir()}
	if info.Mode().IsRegular() {
		file, err := readAssetFile(root, id, rel, ".", tracker)
		if err != nil {
			return ir.LoadedAsset{}, err
		}
		asset.Files = []ir.LoadedAssetFile{file}
		return asset, nil
	}
	if !info.IsDir() {
		return ir.LoadedAsset{}, fmt.Errorf("asset %q path %q: not a regular file or directory", id, declared)
	}
	files, err := loadAssetDir(root, id, rel, tracker)
	if err != nil {
		return ir.LoadedAsset{}, err
	}
	if len(files) == 0 {
		return ir.LoadedAsset{}, fmt.Errorf("asset %q path %q: empty directories are not permitted", id, declared)
	}
	asset.Files = files
	return asset, nil
}

func loadAssetDir(root *os.Root, id, rel string, tracker *assetLimitTracker) ([]ir.LoadedAssetFile, error) {
	var files []ir.LoadedAssetFile
	err := fs.WalkDir(root.FS(), rel, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("asset %q path %q: walk: %w", id, path, walkErr)
		}
		if path == rel {
			return nil
		}
		manifestPath, err := filepath.Rel(rel, path)
		if err != nil {
			return fmt.Errorf("asset %q path %q: manifest path: %w", id, path, err)
		}
		manifestPath = filepath.ToSlash(manifestPath)
		if _, err := assetRelPath(manifestPath); err != nil {
			return fmt.Errorf("asset %q path %q: %w", id, manifestPath, err)
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("asset %q path %q: symlink not permitted", id, manifestPath)
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("asset %q path %q: stat: %w", id, manifestPath, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("asset %q path %q: not a regular file", id, manifestPath)
		}
		file, err := readAssetFile(root, id, path, manifestPath, tracker)
		if err != nil {
			return err
		}
		files = append(files, file)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func readAssetFile(root *os.Root, id, rootPath, manifestPath string, tracker *assetLimitTracker) (ir.LoadedAssetFile, error) {
	fileLimit := tracker.limits.fileBytes
	info, err := root.Lstat(rootPath)
	if err != nil {
		return ir.LoadedAssetFile{}, fmt.Errorf("asset %q path %q: stat: %w", id, manifestPath, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return ir.LoadedAssetFile{}, fmt.Errorf("asset %q path %q: symlink not permitted", id, manifestPath)
	}
	if !info.Mode().IsRegular() {
		return ir.LoadedAssetFile{}, fmt.Errorf("asset %q path %q: not a regular file", id, manifestPath)
	}
	if info.Size() > fileLimit {
		return ir.LoadedAssetFile{}, fmt.Errorf("asset %q path %q: file size %d exceeds limit %d", id, manifestPath, info.Size(), fileLimit)
	}
	f, err := root.Open(rootPath)
	if err != nil {
		return ir.LoadedAssetFile{}, fmt.Errorf("asset %q path %q: open: %w", id, manifestPath, err)
	}
	defer func() { _ = f.Close() }()
	postInfo, err := f.Stat()
	if err != nil {
		return ir.LoadedAssetFile{}, fmt.Errorf("asset %q path %q: stat open file: %w", id, manifestPath, err)
	}
	if !postInfo.Mode().IsRegular() {
		return ir.LoadedAssetFile{}, fmt.Errorf("asset %q path %q: opened file is not regular", id, manifestPath)
	}
	if postInfo.Size() > fileLimit {
		return ir.LoadedAssetFile{}, fmt.Errorf("asset %q path %q: file size %d exceeds limit %d", id, manifestPath, postInfo.Size(), fileLimit)
	}
	if err := tracker.add(id, manifestPath, postInfo.Size()); err != nil {
		return ir.LoadedAssetFile{}, err
	}
	b, err := io.ReadAll(io.LimitReader(f, fileLimit+1))
	if err != nil {
		return ir.LoadedAssetFile{}, fmt.Errorf("asset %q path %q: read: %w", id, manifestPath, err)
	}
	if int64(len(b)) > fileLimit {
		return ir.LoadedAssetFile{}, fmt.Errorf("asset %q path %q: file size exceeds limit %d", id, manifestPath, fileLimit)
	}
	sum := sha256.Sum256(b)
	return ir.LoadedAssetFile{
		Path:   manifestPath,
		Bytes:  b,
		Size:   int64(len(b)),
		SHA256: hex.EncodeToString(sum[:]),
	}, nil
}

func assetRelPath(declared string) (string, error) {
	return safeRootRelPath(declared, safePathPolicy{kind: "asset", allowDot: true})
}

func rejectSymlinkComponents(root *os.Root, rel string) error {
	if rel == "." {
		return nil
	}
	parts := strings.Split(rel, "/")
	for i := range parts {
		p := strings.Join(parts[:i+1], "/")
		info, err := root.Lstat(p)
		if err != nil {
			return fmt.Errorf("stat path component %q: %w", p, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("symlink not permitted at path component %q: %w", p, errSymlinkComponent)
		}
	}
	return nil
}
