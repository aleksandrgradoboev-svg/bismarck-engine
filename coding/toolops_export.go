package coding

import "github.com/aleksandrgradoboev-svg/bismarck-engine/agent"

// The constructors below build a built-in tool against host-supplied file
// operations, as pi's createReadTool(cwd, { operations }) and its siblings do:
// a host that keeps the agent inside a directory, or reads a remote
// filesystem, passes the tool through SessionOptions.Tools or CustomTools.
// The tool resolves its path arguments against cwd itself and hands the
// operations absolute paths, so a host that checks those paths checks what
// the tool will actually touch. A nil ops uses the local filesystem; a non-nil
// one is used whole, with no local fallback for a member left out (see
// resolveReadOperations).
//
// The read tool built here does not resize images: the session's resizer is
// the session's own (createTool).

// CreateReadTool builds the read tool in cwd against ops.
func CreateReadTool(cwd string, ops *ReadOperations) agent.AgentTool {
	return readToolOps(cwd, ops, nil)
}

// CreateLsTool builds the ls tool in cwd against ops.
func CreateLsTool(cwd string, ops *LsOperations) agent.AgentTool {
	return lsToolOps(cwd, ops)
}

// CreateWriteTool builds the write tool in cwd against ops.
func CreateWriteTool(cwd string, ops *WriteOperations) agent.AgentTool {
	return writeToolOps(cwd, ops)
}

// CreateEditTool builds the edit tool in cwd against ops.
func CreateEditTool(cwd string, ops *EditOperations) agent.AgentTool {
	return editToolOps(cwd, ops)
}
