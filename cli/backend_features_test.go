package cli

import (
	"testing"

	"github.com/valbaudo/awf/ir"
)

// F5 (U4): explicit --backend native must fail closed on cmd:/keepalive: —
// neither has a host equivalent (a cmd: service is a keepalive sidecar;
// keepalive: false asks native to not keep a container alive, but native has
// no container to begin with). firstNativeIncompatibleFeature is the pure
// detector selectRunBackend consults under explicit native; these tests
// exercise it directly. (firstNativeIncompatibleFeature does not validate the
// image reference itself, so a placeholder digest string is fine here.)

func TestNativeIncompatible_CmdRejected(t *testing.T) {
	t.Parallel()
	wf := &ir.Workflow{Containers: map[string]ir.Container{
		"svc": {Image: "x@sha256:...", Cmd: []string{"sleep", "infinity"}},
	}}
	f, ok := firstNativeIncompatibleFeature(wf)
	if !ok || f.Kind != "cmd" {
		t.Fatalf("cmd: must be native-incompatible, got %+v ok=%v", f, ok)
	}
	if f.Path != "containers.svc.cmd" {
		t.Errorf("Path = %q, want %q", f.Path, "containers.svc.cmd")
	}
}

func TestNativeIncompatible_KeepaliveFalseRejected(t *testing.T) {
	t.Parallel()
	keepaliveFalse := false
	wf := &ir.Workflow{Containers: map[string]ir.Container{
		"svc": {Image: "x@sha256:...", Keepalive: &keepaliveFalse},
	}}
	f, ok := firstNativeIncompatibleFeature(wf)
	if !ok || f.Kind != "keepalive" {
		t.Fatalf("keepalive: false must be native-incompatible, got %+v ok=%v", f, ok)
	}
	if f.Path != "containers.svc.keepalive" {
		t.Errorf("Path = %q, want %q", f.Path, "containers.svc.keepalive")
	}
}

// keepalive: true (or unset) is not native-incompatible on its own — only an
// explicit false (native genuinely cannot honor "keep this container alive"
// since there's no container) is rejected.
func TestNativeIncompatible_KeepaliveTrueAccepted(t *testing.T) {
	t.Parallel()
	keepaliveTrue := true
	wf := &ir.Workflow{Containers: map[string]ir.Container{
		"svc": {Image: "x@sha256:...", Keepalive: &keepaliveTrue},
	}}
	if f, ok := firstNativeIncompatibleFeature(wf); ok {
		t.Fatalf("keepalive: true must not be native-incompatible, got %+v", f)
	}
}
