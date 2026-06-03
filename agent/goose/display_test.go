package goose_test

import (
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/goose"
)

func TestDisplayForGoose_Classes(t *testing.T) {
	asst, _ := goose.ParseStreamEventForTest(msgLine("assistant", "hi"))
	if d := goose.DisplayForGooseForTest(asst); d.Class != agent.DisplayAssistant || d.Text != "hi" {
		t.Errorf("assistant display = %+v", d)
	}
	cmpl, _ := goose.ParseStreamEventForTest(completeLine(0, 0))
	if d := goose.DisplayForGooseForTest(cmpl); d.Class != agent.DisplayFinal {
		t.Errorf("complete display class = %v, want DisplayFinal", d.Class)
	}
	errEv, _ := goose.ParseStreamEventForTest([]byte(`{"type":"error","error":"boom"}`))
	if d := goose.DisplayForGooseForTest(errEv); d.Class != agent.DisplayError || !d.IsError {
		t.Errorf("error display = %+v", d)
	}
}
