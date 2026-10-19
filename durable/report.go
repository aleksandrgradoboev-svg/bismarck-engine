package durable

// ReportType says what a report to the Harness's OnReport is about.
type ReportType string

const (
	// ReportSessionFailed: the error failed the Session, which closes: a
	// Storage error, often a platform reset.
	ReportSessionFailed ReportType = "session_failed"
	// ReportTaskFaulted: the task ended faulted: its phase failed (Reason
	// "threw"), or its abort handler was cut off the restart limit's times
	// (Reason "restart_limit").
	ReportTaskFaulted ReportType = "task_faulted"
	// ReportTaskParked: the restart limit parked the task until RetryTask.
	ReportTaskParked ReportType = "task_parked"
	// ReportMigrationFailed: the task's stored state could not be migrated;
	// it is blocked until the registry changes.
	ReportMigrationFailed ReportType = "migration_failed"
	// ReportTaskOutdated: a running task keeps running under its old
	// definition, which was uninstalled (Reason "missing_task") or replaced
	// by one that cannot take over its state (Reason "incompatible_task").
	ReportTaskOutdated ReportType = "task_outdated"
	// ReportTaskError: the task's own TaskRuntime.Report, or a failure in
	// running it.
	ReportTaskError ReportType = "task_error"
	// ReportExtensionError: a hook, a section, a wrapper, or a tool's invalid
	// structured output; with the task that ran it, or, outside a task, the
	// conversation whose agent was resolved.
	ReportExtensionError ReportType = "extension_error"
	// ReportHostError: a host callback failed, named by Callback; with the
	// task that called it, if one did.
	ReportHostError ReportType = "host_error"
	// ReportListenerError: a commit, close, watch, view, or document listener
	// failed.
	ReportListenerError ReportType = "listener_error"
)

// Host callbacks a ReportHostError names, by their pi HarnessOptions names:
// env is the environment builder, models a provider call.
const (
	CallbackNow      = "now"
	CallbackSettings = "settings"
	CallbackRegistry = "registry"
	CallbackEnv      = "env"
	CallbackModels   = "models"
	CallbackSetTimer = "setTimer"
	CallbackOnWake   = "onWake"
)

// ReportTask is the task a report is about.
type ReportTask struct {
	ID TaskID `json:"id"`
	// Kind is the task's registered name.
	Kind           string         `json:"kind"`
	ConversationID ConversationID `json:"conversationId"`
}

// ReportInfo says where a reported failure came from. Type decides which
// other fields are set. Extension, where set, names the extension whose code
// failed, when known; for a task's own code, the one that installed the
// definition the task ran.
type ReportInfo struct {
	Type ReportType  `json:"type"`
	Task *ReportTask `json:"task,omitempty"`
	// Extension is set for task and extension reports, when known.
	Extension string `json:"extension,omitempty"`
	// ConversationID is set for an extension error outside a task.
	ConversationID ConversationID `json:"conversationId,omitempty"`
	// Reason is set for task_faulted and task_outdated.
	Reason string `json:"reason,omitempty"`
	// Callback is set for host_error.
	Callback string `json:"callback,omitempty"`
}

// ListenerError is the info of a listener's failure, the default of a
// report without one.
var ListenerError = ReportInfo{Type: ReportListenerError}

// HostError is the info of a failed host callback, with the task that called
// it, or nil.
func HostError(callback string, task *ReportTask) ReportInfo {
	return ReportInfo{Type: ReportHostError, Callback: callback, Task: task}
}
