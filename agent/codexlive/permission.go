package codexlive

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/valbaudo/awf/agent"
)

type permissionPolicy struct {
	rules []permissionRule
}

type permissionRule struct {
	kind      string
	toolID    string
	pathRoots []string
}

func parsePermissionPolicy(raw any) (permissionPolicy, error) {
	if raw == nil {
		return permissionPolicy{}, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return permissionPolicy{}, &agent.ErrInvalidConfig{Ref: AdapterRef, Key: "permission_policy", Reason: fmt.Sprintf("must be object, got %T", raw)}
	}
	for k := range m {
		if k != "allow" {
			return permissionPolicy{}, &agent.ErrInvalidConfig{Ref: AdapterRef, Key: "permission_policy." + k, Reason: "unknown key", KeyUnknown: true}
		}
	}
	allow, ok := m["allow"]
	if !ok {
		return permissionPolicy{}, nil
	}
	items, ok := allow.([]any)
	if !ok {
		return permissionPolicy{}, &agent.ErrInvalidConfig{Ref: AdapterRef, Key: "permission_policy.allow", Reason: fmt.Sprintf("must be array, got %T", allow)}
	}
	p := permissionPolicy{rules: make([]permissionRule, 0, len(items))}
	for i, item := range items {
		rule, err := parsePermissionRule(i, item)
		if err != nil {
			return permissionPolicy{}, err
		}
		p.rules = append(p.rules, rule)
	}
	return p, nil
}

func parsePermissionRule(i int, raw any) (permissionRule, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return permissionRule{}, &agent.ErrInvalidConfig{Ref: AdapterRef, Key: fmt.Sprintf("permission_policy.allow[%d]", i), Reason: fmt.Sprintf("must be object, got %T", raw)}
	}
	for k := range m {
		switch k {
		case "kind", "tool_id", "path_roots":
		case "command_prefix", "command_prefixes":
			return permissionRule{}, &agent.ErrInvalidConfig{Ref: AdapterRef, Key: fmt.Sprintf("permission_policy.allow[%d].%s", i, k), Reason: "command prefixes are not a security boundary"}
		default:
			return permissionRule{}, &agent.ErrInvalidConfig{Ref: AdapterRef, Key: fmt.Sprintf("permission_policy.allow[%d].%s", i, k), Reason: "unknown key", KeyUnknown: true}
		}
	}
	kind, ok := m["kind"].(string)
	if !ok || kind == "" {
		return permissionRule{}, &agent.ErrInvalidConfig{Ref: AdapterRef, Key: fmt.Sprintf("permission_policy.allow[%d].kind", i), Reason: "required string"}
	}
	rule := permissionRule{kind: kind}
	if v, ok := m["tool_id"]; ok {
		s, ok := v.(string)
		if !ok {
			return permissionRule{}, &agent.ErrInvalidConfig{Ref: AdapterRef, Key: fmt.Sprintf("permission_policy.allow[%d].tool_id", i), Reason: fmt.Sprintf("must be string, got %T", v)}
		}
		rule.toolID = s
	}
	if v, ok := m["path_roots"]; ok {
		items, ok := v.([]any)
		if !ok {
			return permissionRule{}, &agent.ErrInvalidConfig{Ref: AdapterRef, Key: fmt.Sprintf("permission_policy.allow[%d].path_roots", i), Reason: fmt.Sprintf("must be array, got %T", v)}
		}
		for j, item := range items {
			root, ok := item.(string)
			if !ok || root == "" {
				return permissionRule{}, &agent.ErrInvalidConfig{Ref: AdapterRef, Key: fmt.Sprintf("permission_policy.allow[%d].path_roots[%d]", i, j), Reason: "must be non-empty string"}
			}
			canonical, err := canonicalExistingRoot(root)
			if err != nil {
				return permissionRule{}, &agent.ErrInvalidConfig{Ref: AdapterRef, Key: fmt.Sprintf("permission_policy.allow[%d].path_roots[%d]", i, j), Reason: err.Error()}
			}
			rule.pathRoots = append(rule.pathRoots, canonical)
		}
	}
	return rule, nil
}

func (p permissionPolicy) allows(req *PermissionRequest) bool {
	if req == nil {
		return false
	}
	for _, rule := range p.rules {
		if rule.kind != req.Kind {
			continue
		}
		if rule.toolID != "" && rule.toolID != req.ToolID {
			continue
		}
		if len(rule.pathRoots) > 0 && !pathUnderAnyRoot(req.Path, rule.pathRoots) {
			continue
		}
		return true
	}
	return false
}

func pathUnderAnyRoot(path string, roots []string) bool {
	if path == "" {
		return false
	}
	clean, err := canonicalRequestPath(path)
	if err != nil {
		return false
	}
	for _, root := range roots {
		if clean == root || strings.HasPrefix(clean, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func canonicalExistingRoot(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(abs))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", path)
	}
	return filepath.Clean(resolved), nil
}

func canonicalRequestPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	clean := filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		return filepath.Clean(resolved), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	probe := clean
	var suffix []string
	for {
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", os.ErrNotExist
		}
		suffix = append([]string{filepath.Base(probe)}, suffix...)
		resolved, err := filepath.EvalSymlinks(parent)
		if err == nil {
			parts := append([]string{filepath.Clean(resolved)}, suffix...)
			return filepath.Clean(filepath.Join(parts...)), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		probe = parent
	}
}

func permissionReason(allowed bool) string {
	if allowed {
		return "allowed by AWF live permission policy"
	}
	return "denied by AWF live permission policy"
}
