package tools

import (
	"context"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/harness"
)

// NewWriteTool is the write tool (pi createWriteTool): it replaces a file's
// content, creating the file and its parent directories, serialized with
// other edits and writes of the same file.
func NewWriteTool() *harness.ToolRegistration {
	return &harness.ToolRegistration{
		Tool: ai.Tool{
			Name:        "write",
			Description: "Write content to a file. Creates the file if it doesn't exist, overwrites if it does. Automatically creates parent directories.",
			Parameters: ai.Object(
				ai.Prop("path", ai.String("Path to the file to write (relative or absolute)")),
				ai.Prop("content", ai.String("Content to write to the file")),
			),
		},
		Execute: func(ctx context.Context, args map[string]any, api harness.ToolExecutionAPI) (harness.ToolExecutionResult, error) {
			path, _ := args["path"].(string)
			content, _ := args["content"].(string)
			e, err := requireEnv(api)
			if err != nil {
				return harness.ToolExecutionResult{}, err
			}
			absolute, err := resolveToolPath(ctx, e, path)
			if err != nil {
				return harness.ToolExecutionResult{}, err
			}
			return withFileMutationQueue(ctx, e, absolute, func() (harness.ToolExecutionResult, error) {
				if ctx.Err() != nil {
					return harness.ToolExecutionResult{}, errAborted
				}
				if err := e.WriteFile(ctx, absolute, []byte(content)); err != nil {
					return harness.ToolExecutionResult{}, err
				}
				if ctx.Err() != nil {
					return harness.ToolExecutionResult{}, errAborted
				}
				return harness.ToolExecutionResult{
					Output: ai.ContentList{ai.TextContent{Text: "Successfully wrote to " + path}},
				}, nil
			})
		},
	}
}
