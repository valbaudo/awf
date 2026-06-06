package ir

import (
	"reflect"
	"strings"
	"testing"
)

// irTypes lists every struct whose fields enter the digest. ADD NEW IR STRUCTS HERE.
func irTypes() []reflect.Type {
	return []reflect.Type{
		reflect.TypeOf(Workflow{}), reflect.TypeOf(Container{}), reflect.TypeOf(Resources{}),
		reflect.TypeOf(RetryPolicy{}),
		reflect.TypeOf(CodeStep{}), reflect.TypeOf(AgentStep{}), reflect.TypeOf(SignalStep{}),
		reflect.TypeOf(If{}), reflect.TypeOf(Loop{}), reflect.TypeOf(Try{}),
		reflect.TypeOf(Parallel{}), reflect.TypeOf(Gate{}), reflect.TypeOf(Skip{}), reflect.TypeOf(Map{}),
		reflect.TypeOf(AgentRole{}),
	}
}

func TestEveryExportedFieldHasNonEmptyJSONTag(t *testing.T) {
	for _, typ := range irTypes() {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if f.PkgPath != "" { // unexported
				continue
			}
			tag, ok := f.Tag.Lookup("json")
			if !ok {
				t.Errorf("%s.%s has no json tag", typ.Name(), f.Name)
				continue
			}
			name := strings.Split(tag, ",")[0]
			if name == "" { // e.g. `json:",omitempty"` — would silently use the Go field name in the digest
				t.Errorf("%s.%s has an empty json tag name (%q)", typ.Name(), f.Name, tag)
			}
		}
	}
}
