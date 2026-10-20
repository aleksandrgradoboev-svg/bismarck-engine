// Package tools is pi-durable's coding tools on an execution environment
// (pi packages/durable/src/tools): read, write, edit and bash take the
// environment from ToolExecutionAPI.Env, stream through its Output, and report
// remarks as diagnostics instead of content text.
package tools

import (
	"errors"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/harness"
)

// requireEnv is the call's execution environment; a tool without one fails
// with an ordinary error result.
func requireEnv(api harness.ToolExecutionAPI) (env.ExecutionEnv, error) {
	e := api.Env()
	if e == nil {
		return nil, errors.New("No execution environment is configured")
	}
	return e, nil
}

// errAborted is a tool's error when its call is cancelled between steps.
var errAborted = errors.New("Operation aborted")
