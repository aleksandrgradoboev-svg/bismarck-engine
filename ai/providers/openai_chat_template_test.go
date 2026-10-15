package providers

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// ctkModel builds a chat-template model with the given ordered kwargs JSON and
// thinkingLevelMap. thinkingFormat:"chat-template" overrides the detected format.
func ctkModel(kwargsJSON string, tlm ai.ThinkingLevelMap) *ai.Model {
	return openAIModel(func(m *ai.Model) {
		m.ID = "custom-chat-template"
		m.Reasoning = true
		m.ThinkingLevelMap = tlm
		m.Compat = json.RawMessage(`{"thinkingFormat":"chat-template","chatTemplateKwargs":` + kwargsJSON + `}`)
	})
}

func ctkParam(t *testing.T, body map[string]any) string {
	t.Helper()
	v, ok := body["chat_template_kwargs"]
	if !ok {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal chat_template_kwargs: %v", err)
	}
	return string(b)
}

// TestOpenAIChatTemplateKwargs pins pi's chat-template thinking compat
// (upstream 8b97e75c): $var resolution, omitWhenOff, scalar pass-through
// (including null), thinkingLevelMap mapping, and insertion-order preservation.
// The {"$var":"thinking.budget"} case lives in openai_thinking_token_budget_test.go,
// where upstream b23741269 put its tests.
func TestOpenAIChatTemplateKwargs(t *testing.T) {
	// Ordered: enabled var, effort var (omitWhenOff), literal scalar, null scalar.
	kwargs := `{"thinking":{"$var":"thinking.enabled"},"reasoning_effort":{"$var":"thinking.effort","omitWhenOff":true},"literal":"x","nullable":null}`
	tlm := ai.ThinkingLevelMap{
		"off":     strPtr("none"),
		"minimal": nil,
		"low":     strPtr("low"),
		"high":    strPtr("high"),
	}

	// Effort high: enabled->true, effort->mapped "high", scalars carried, order kept.
	bHigh := mustBuildOpenAIParams(t, ctkModel(kwargs, tlm), baseReq(), &OpenAIOptions{ReasoningEffort: "high"})
	if got, want := ctkParam(t, bHigh), `{"thinking":true,"reasoning_effort":"high","literal":"x","nullable":null}`; got != want {
		t.Fatalf("high: chat_template_kwargs = %s, want %s", got, want)
	}

	// Off: enabled->false, effort omitted via omitWhenOff, scalars remain.
	bOff := mustBuildOpenAIParams(t, ctkModel(kwargs, tlm), baseReq(), &OpenAIOptions{})
	if got, want := ctkParam(t, bOff), `{"thinking":false,"literal":"x","nullable":null}`; got != want {
		t.Fatalf("off: chat_template_kwargs = %s, want %s", got, want)
	}

	// minimal maps to null -> effort key omitted (thinking still true).
	bMin := mustBuildOpenAIParams(t, ctkModel(kwargs, tlm), baseReq(), &OpenAIOptions{ReasoningEffort: "minimal"})
	if got, want := ctkParam(t, bMin), `{"thinking":true,"literal":"x","nullable":null}`; got != want {
		t.Fatalf("minimal: chat_template_kwargs = %s, want %s", got, want)
	}
}

// TestOpenAIChatTemplateEffortFallbacks covers the thinking.effort lookup edges:
// unmapped level falls back to the raw level; off with a mapped string sends it;
// off with no entry omits.
func TestOpenAIChatTemplateEffortFallbacks(t *testing.T) {
	kwargs := `{"effort":{"$var":"thinking.effort"}}`

	// Unmapped level "medium" (not in map) -> raw level string.
	bMed := mustBuildOpenAIParams(t, ctkModel(kwargs, ai.ThinkingLevelMap{"high": strPtr("high")}),
		baseReq(), &OpenAIOptions{ReasoningEffort: "medium"})
	if got, want := ctkParam(t, bMed), `{"effort":"medium"}`; got != want {
		t.Fatalf("unmapped level: %s, want %s", got, want)
	}

	// Off with tlm.off mapped to a string -> sent (no omitWhenOff).
	bOffStr := mustBuildOpenAIParams(t, ctkModel(kwargs, ai.ThinkingLevelMap{"off": strPtr("none")}),
		baseReq(), &OpenAIOptions{})
	if got, want := ctkParam(t, bOffStr), `{"effort":"none"}`; got != want {
		t.Fatalf("off mapped string: %s, want %s", got, want)
	}

	// Off with no tlm.off entry -> reasoningEffort undefined -> whole object omitted.
	bOffNone := mustBuildOpenAIParams(t, ctkModel(kwargs, ai.ThinkingLevelMap{"high": strPtr("high")}),
		baseReq(), &OpenAIOptions{})
	if has(bOffNone, "chat_template_kwargs") {
		t.Fatalf("off with no off-entry must omit chat_template_kwargs, got %s", ctkParam(t, bOffNone))
	}
}

// basetenModel builds a thinkingFormat:"baseten" model with the given ordered
// chatTemplateArgs JSON, thinkingLevelMap, and supportsReasoningEffort override.
func basetenModel(argsJSON string, tlm ai.ThinkingLevelMap, supportsEffort bool) *ai.Model {
	return openAIModel(func(m *ai.Model) {
		m.ID = "custom-baseten"
		m.Reasoning = true
		m.ThinkingLevelMap = tlm
		m.Compat = json.RawMessage(`{"thinkingFormat":"baseten","supportsReasoningEffort":` +
			strconv.FormatBool(supportsEffort) + `,"chatTemplateArgs":` + argsJSON + `}`)
	})
}

func basetenArgs(t *testing.T, body map[string]any) string {
	t.Helper()
	v, ok := body["chat_template_args"]
	if !ok {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal chat_template_args: %v", err)
	}
	return string(b)
}

// TestOpenAIBasetenChatTemplateArgs pins pi's baseten thinking compat (upstream
// c1019d92): configurable chat_template_args resolved exactly like
// chat_template_kwargs, emitted under the distinct `chat_template_args` key.
func TestOpenAIBasetenChatTemplateArgs(t *testing.T) {
	args := `{"thinking":{"$var":"thinking.enabled"},"effort":{"$var":"thinking.effort","omitWhenOff":true},"literal":"x"}`
	tlm := ai.ThinkingLevelMap{"off": strPtr("none"), "high": strPtr("high")}

	bHigh := mustBuildOpenAIParams(t, basetenModel(args, tlm, true), baseReq(), &OpenAIOptions{ReasoningEffort: "high"})
	if got, want := basetenArgs(t, bHigh), `{"thinking":true,"effort":"high","literal":"x"}`; got != want {
		t.Fatalf("high: chat_template_args = %s, want %s", got, want)
	}
	// The kwargs key belongs to the "chat-template" format only.
	if has(bHigh, "chat_template_kwargs") {
		t.Fatalf("baseten must not emit chat_template_kwargs, got %v", bHigh["chat_template_kwargs"])
	}

	bOff := mustBuildOpenAIParams(t, basetenModel(args, tlm, true), baseReq(), &OpenAIOptions{})
	if got, want := basetenArgs(t, bOff), `{"thinking":false,"literal":"x"}`; got != want {
		t.Fatalf("off: chat_template_args = %s, want %s", got, want)
	}

	// Nothing survives resolution -> no chat_template_args param at all.
	bNone := mustBuildOpenAIParams(t, basetenModel(`{"effort":{"$var":"thinking.effort","omitWhenOff":true}}`, tlm, true),
		baseReq(), &OpenAIOptions{})
	if has(bNone, "chat_template_args") {
		t.Fatalf("all-omitted must drop chat_template_args, got %s", basetenArgs(t, bNone))
	}
}

// TestOpenAIBasetenReasoningEffort pins the baseten reasoning_effort lookup:
// requested effort resolves through thinkingLevelMap (absent -> raw level,
// present-null -> omitted); with no requested effort only a mapped-string `off`
// is sent; supportsReasoningEffort:false drops the field entirely.
func TestOpenAIBasetenReasoningEffort(t *testing.T) {
	const noArgs = `{}`
	tlm := ai.ThinkingLevelMap{"off": strPtr("none"), "minimal": nil, "high": strPtr("baseten-high")}

	effort := func(body map[string]any) any { return body["reasoning_effort"] }

	// Mapped level -> mapped string.
	bHigh := mustBuildOpenAIParams(t, basetenModel(noArgs, tlm, true), baseReq(), &OpenAIOptions{ReasoningEffort: "high"})
	if got := effort(bHigh); got != "baseten-high" {
		t.Fatalf("high: reasoning_effort = %v, want baseten-high", got)
	}

	// Level absent from the map -> raw requested level.
	bMed := mustBuildOpenAIParams(t, basetenModel(noArgs, tlm, true), baseReq(), &OpenAIOptions{ReasoningEffort: "medium"})
	if got := effort(bMed); got != "medium" {
		t.Fatalf("medium: reasoning_effort = %v, want medium", got)
	}

	// Level mapped to null -> field omitted (pi's typeof-string guard).
	bMin := mustBuildOpenAIParams(t, basetenModel(noArgs, tlm, true), baseReq(), &OpenAIOptions{ReasoningEffort: "minimal"})
	if has(bMin, "reasoning_effort") {
		t.Fatalf("minimal maps to null, reasoning_effort must be omitted, got %v", effort(bMin))
	}

	// No requested effort -> thinkingLevelMap.off string is sent.
	bOff := mustBuildOpenAIParams(t, basetenModel(noArgs, tlm, true), baseReq(), &OpenAIOptions{})
	if got := effort(bOff); got != "none" {
		t.Fatalf("off: reasoning_effort = %v, want none", got)
	}

	// No requested effort and no off entry -> nothing to fall back to.
	bNoOff := mustBuildOpenAIParams(t, basetenModel(noArgs, ai.ThinkingLevelMap{"high": strPtr("high")}, true),
		baseReq(), &OpenAIOptions{})
	if has(bNoOff, "reasoning_effort") {
		t.Fatalf("absent off entry must omit reasoning_effort, got %v", effort(bNoOff))
	}

	// off mapped to null -> omitted as well.
	bOffNull := mustBuildOpenAIParams(t, basetenModel(noArgs, ai.ThinkingLevelMap{"off": nil}, true),
		baseReq(), &OpenAIOptions{})
	if has(bOffNull, "reasoning_effort") {
		t.Fatalf("null off mapping must omit reasoning_effort, got %v", effort(bOffNull))
	}

	// supportsReasoningEffort:false -> never sent, even with a mapped level.
	bUnsupported := mustBuildOpenAIParams(t, basetenModel(noArgs, tlm, false), baseReq(), &OpenAIOptions{ReasoningEffort: "high"})
	if has(bUnsupported, "reasoning_effort") {
		t.Fatalf("supportsReasoningEffort:false must omit reasoning_effort, got %v", effort(bUnsupported))
	}
}

// TestOpenAIChatTemplateAllOmitted: when every kwarg resolves to omitted, pi
// returns undefined and no chat_template_kwargs param is emitted.
func TestOpenAIChatTemplateAllOmitted(t *testing.T) {
	kwargs := `{"effort":{"$var":"thinking.effort","omitWhenOff":true}}`
	body := mustBuildOpenAIParams(t, ctkModel(kwargs, ai.ThinkingLevelMap{"off": strPtr("none")}),
		baseReq(), &OpenAIOptions{})
	if has(body, "chat_template_kwargs") {
		t.Fatalf("all-omitted must drop chat_template_kwargs, got %s", ctkParam(t, body))
	}
}
