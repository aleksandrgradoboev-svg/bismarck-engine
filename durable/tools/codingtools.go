package tools

import "github.com/aleksandrgradoboev-svg/bismarck-engine/durable/harness"

// NewCodingTools is the coding-tools extension (pi createCodingTools): read,
// write, edit and bash; nothing installs it automatically. NewPowerShellTool
// adds powershell. options.Images prepares the images read returns. Each call
// builds a fresh extension, so no shared value can be changed for every
// registry at once.
func NewCodingTools(options ReadToolOptions) *harness.Extension {
	return &harness.Extension{
		Name:  "coding-tools",
		Tools: []*harness.ToolRegistration{NewReadTool(options), NewWriteTool(), NewEditTool(), NewBashTool(BashToolOptions{})},
	}
}
