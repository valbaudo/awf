package cli_test

import "github.com/valbaudo/awf/container"

const claudeSuccessStream = `{"type":"result","subtype":"success","is_error":false,"num_turns":1,"structured_output":{}}
`

func programClaudeSuccess(fake *container.Fake) {
	stream := []byte(claudeSuccessStream)
	fake.ProgramExec("claude --version", container.ExecResult{ExitCode: 0, Stdout: []byte("2.1.0\n")}, nil)
	fake.ProgramExecAny(container.ExecResult{ExitCode: 0, Stdout: stream}, []container.IOChunk{
		{Stream: "stdout", Data: stream},
	})
}
