package droid_test

import (
	"testing"

	"github.com/valbaudo/awf/agent/droid"
)

func TestNew_Defaults(t *testing.T) {
	a, err := droid.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if droid.AdapterRef != "factory/droid" {
		t.Errorf("AdapterRef = %q, want factory/droid", droid.AdapterRef)
	}
	if a.Ref() != droid.AdapterRef {
		t.Errorf("Ref() = %q, want %q", a.Ref(), droid.AdapterRef)
	}
}

func TestCapabilities_NativeSchemaFalse(t *testing.T) {
	a, _ := droid.New()
	if a.Capabilities().NativeSchema {
		t.Error("Capabilities().NativeSchema = true, want false (droid has no native --json-schema)")
	}
}

func TestDefaultEnvAllowlist_HasFactoryKey(t *testing.T) {
	found := false
	for _, k := range droid.DefaultEnvAllowlist {
		if k == "FACTORY_API_KEY" {
			found = true
		}
	}
	if !found {
		t.Errorf("DefaultEnvAllowlist = %v, want it to contain FACTORY_API_KEY", droid.DefaultEnvAllowlist)
	}
}
