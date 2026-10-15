// Go side of the differential: build each scenario's request body with the Go
// port and write it out canonically, byte-for-byte comparable with the pi side.
//
// Capture is the mirror of pi's onPayload-throws trick: OnPayload stores the
// body and returns an error, which fails the stream before any network call.
// No API key is needed or used.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
)

// ---------------------------------------------------------------------------
// Scenario
// ---------------------------------------------------------------------------

type scenario struct {
	Name         string          `json:"name"`
	Backend      string          `json:"backend"`
	API          string          `json:"api"`
	Entry        string          `json:"entry"`
	Model        json.RawMessage `json:"model"`
	Context      json.RawMessage `json:"context"`
	Options      json.RawMessage `json:"options"`
	ComparePaths []string        `json:"comparePaths"`
}

// project restricts a payload to the declared top-level paths ("$.contents").
//
// This exists for ONE reason: google-generative-ai's payload hook observes a
// different layer on each side. pi hands the @google/genai SDK a call-params
// object ({model, contents, config}) and the SDK builds the REST body inside
// itself — pi explicitly refuses a custom fetch, so the real REST body is not
// observable from pi at all. The Go port has no such SDK and builds the REST
// body directly. Only `contents` is the same artifact on both sides, so the
// google scenarios compare that and NOTHING ELSE. Everything outside it is out
// of the differential's reach — see README "Known limits".
func project(payload any, comparePaths []string) (any, error) {
	if len(comparePaths) == 0 {
		return payload, nil
	}
	m, ok := payload.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("comparePaths needs a map payload, got %T", payload)
	}
	out := map[string]any{}
	for _, p := range comparePaths {
		key, ok := strings.CutPrefix(p, "$.")
		if !ok || strings.ContainsAny(key, ".[") {
			return nil, fmt.Errorf(`comparePaths only supports "$.<topLevelKey>", got %q`, p)
		}
		v, present := m[key]
		if !present {
			return nil, fmt.Errorf("comparePaths %s: key absent from payload", p)
		}
		out[key] = v
	}
	return out, nil
}

// rawContext mirrors pi's Context on the wire; Messages needs the
// role-discriminated decoding that ai.UnmarshalMessage provides (system
// messages included).
type rawContext struct {
	SystemPrompt string            `json:"systemPrompt"`
	Messages     []json.RawMessage `json:"messages"`
	Tools        []ai.Tool         `json:"tools"`
}

func decodeContext(raw json.RawMessage) (ai.Context, error) {
	var rc rawContext
	if err := json.Unmarshal(raw, &rc); err != nil {
		return ai.Context{}, fmt.Errorf("context: %w", err)
	}
	out := ai.Context{SystemPrompt: rc.SystemPrompt, Tools: rc.Tools}
	for i, m := range rc.Messages {
		msg, err := ai.UnmarshalMessage(m)
		if err != nil {
			return ai.Context{}, fmt.Errorf("context.messages[%d]: %w", i, err)
		}
		out.Messages = append(out.Messages, msg)
	}
	return out, nil
}

// scenarioOptions is the pi-shaped (camelCase) option surface the scenarios
// speak. The Go StreamOptions struct carries no JSON tags, so the mapping onto
// it is written out explicitly here — that keeps it auditable, and an unknown
// key in a scenario becomes a hard error instead of a silent no-op.
type scenarioOptions struct {
	APIKey         *string        `json:"apiKey"`
	Temperature    *float64       `json:"temperature"`
	MaxTokens      *int           `json:"maxTokens"`
	SessionID      *string        `json:"sessionId"`
	CacheRetention *string        `json:"cacheRetention"`
	SamplingParams map[string]any `json:"samplingParams"`
	Metadata       map[string]any `json:"metadata"`

	// SimpleStreamOptions only.
	Reasoning       *string             `json:"reasoning"`
	ThinkingBudgets *ai.ThinkingBudgets `json:"thinkingBudgets"`

	// The native `stream` entries (openai-completions, openai-responses) only.
	ReasoningEffort *string `json:"reasoningEffort"`
	// ToolChoice serves both entries: the native `stream` one takes it verbatim,
	// and the simple entry narrows it to pi's "auto"/"none" (upstream e5dde9a76).
	ToolChoice any `json:"toolChoice"`

	// Server-side refusal fallback has NO option: upstream ed867e909 withdrew
	// `refusalFallbacks` from SimpleStreamOptions entirely. Both sides now derive
	// the request's `fallbacks` field from `model.compat.allowedFallbackModels`,
	// so the scenarios drive it through the model blob — which is also why
	// DisallowUnknownFields matters here: a scenario still carrying the retired
	// option is a hard error rather than a silently ignored key.
}

func decodeOptions(raw json.RawMessage) (scenarioOptions, error) {
	var o scenarioOptions
	if len(raw) == 0 {
		return o, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		return o, fmt.Errorf("options: %w (add the field to scenarioOptions in go/main.go if pi supports it)", err)
	}
	return o, nil
}

func (o scenarioOptions) base() ai.StreamOptions {
	s := ai.StreamOptions{
		Temperature:    o.Temperature,
		MaxTokens:      o.MaxTokens,
		SamplingParams: o.SamplingParams,
		Metadata:       o.Metadata,
	}
	if o.APIKey != nil {
		s.APIKey = *o.APIKey
	}
	if o.SessionID != nil {
		s.SessionID = *o.SessionID
	}
	if o.CacheRetention != nil {
		s.CacheRetention = ai.CacheRetention(*o.CacheRetention)
	}
	return s
}

func (o scenarioOptions) simple() ai.SimpleStreamOptions {
	s := ai.SimpleStreamOptions{StreamOptions: o.base(), ThinkingBudgets: o.ThinkingBudgets}
	if o.Reasoning != nil {
		s.Reasoning = ai.ThinkingLevel(*o.Reasoning)
	}
	if tc, ok := o.ToolChoice.(string); ok {
		s.ToolChoice = ai.ToolChoice(tc)
	}
	return s
}

// ---------------------------------------------------------------------------
// Capture
// ---------------------------------------------------------------------------

var errHalt = errors.New("payload captured")

// capture runs a stream entry point far enough to build the request body and
// returns it. OnPayload's error aborts the stream before any network call.
//
// The adapter entry points take a normalized transcript, so the scenario's
// context is normalized first — what the pi arm does for a "src" scenario at a
// sha that has normalizeContext, and what the public entry points do on both
// sides. For a "dist" scenario pi's adapter takes the raw context, which carries
// no system messages there, and normalizing it changes nothing it sends.
func capture(sc scenario, model *ai.Model, raw ai.Context, o scenarioOptions) (any, error) {
	req := ai.NormalizeContext(raw)
	var captured any
	hook := func(payload any, _ *ai.Model) (any, error) {
		captured = payload
		return nil, errHalt
	}

	simple := sc.Entry == "streamSimple"
	var final *ai.AssistantMessage

	switch {
	case sc.API == "openai-completions" && simple:
		opts := o.simple()
		opts.OnPayload = hook
		final = providers.StreamSimpleOpenAICompletions(context.Background(), model, req, &opts).Result()

	case sc.API == "openai-completions":
		opts := &providers.OpenAIOptions{StreamOptions: o.base(), ToolChoice: o.ToolChoice}
		if o.ReasoningEffort != nil {
			opts.ReasoningEffort = *o.ReasoningEffort
		}
		opts.OnPayload = hook
		final = providers.StreamOpenAICompletions(context.Background(), model, req, opts).Result()

	case sc.API == "google-generative-ai" && simple:
		opts := o.simple()
		opts.OnPayload = hook
		final = providers.StreamSimpleGoogle(context.Background(), model, req, &opts).Result()

	case sc.API == "anthropic-messages" && simple:
		opts := o.simple()
		opts.OnPayload = hook
		final = providers.StreamSimpleAnthropic(context.Background(), model, req, &opts).Result()

	case sc.API == "openai-responses" && simple:
		opts := o.simple()
		opts.OnPayload = hook
		final = providers.StreamSimpleOpenAIResponses(context.Background(), model, req, &opts).Result()

	case sc.API == "openai-responses":
		opts := &providers.OpenAIResponsesOptions{StreamOptions: o.base(), ToolChoice: o.ToolChoice}
		if o.ReasoningEffort != nil {
			opts.ReasoningEffort = *o.ReasoningEffort
		}
		opts.OnPayload = hook
		final = providers.StreamOpenAIResponses(context.Background(), model, req, opts).Result()

	default:
		return nil, fmt.Errorf("no Go dispatch for api=%q entry=%q (add one to capture() in go/main.go)", sc.API, sc.Entry)
	}

	if captured == nil {
		if final == nil {
			return nil, fmt.Errorf("payload was never built (no final message)")
		}
		return nil, fmt.Errorf("payload was never built (stream ended %s: %q)", final.StopReason, final.ErrorMessage)
	}
	return captured, nil
}

// ---------------------------------------------------------------------------

func run() error {
	root, err := filepath.Abs("..")
	if err != nil {
		return err
	}
	scenariosDir := filepath.Join(root, "scenarios")
	outDir := filepath.Join(root, "out", "go")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	only := os.Getenv("DIFF_ONLY")

	entries, err := os.ReadDir(scenariosDir)
	if err != nil {
		return err
	}
	names := []string{}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	failures := 0
	for _, file := range names {
		data, err := os.ReadFile(filepath.Join(scenariosDir, file))
		if err != nil {
			return err
		}
		var sc scenario
		if err := json.Unmarshal(data, &sc); err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		if only != "" && sc.Name != only {
			continue
		}
		if err := one(sc, outDir); err != nil {
			failures++
			fmt.Fprintf(os.Stderr, "go  ERROR %s: %v\n", sc.Name, err)
			continue
		}
		fmt.Printf("go  ok    %s\n", sc.Name)
	}
	if failures > 0 {
		return fmt.Errorf("%d scenario(s) failed to build", failures)
	}
	return nil
}

func one(sc scenario, outDir string) error {
	var model ai.Model
	if err := json.Unmarshal(sc.Model, &model); err != nil {
		return fmt.Errorf("model: %w", err)
	}
	req, err := decodeContext(sc.Context)
	if err != nil {
		return err
	}
	opts, err := decodeOptions(sc.Options)
	if err != nil {
		return err
	}
	payload, err := capture(sc, &model, req, opts)
	if err != nil {
		return err
	}
	payload, err = project(payload, sc.ComparePaths)
	if err != nil {
		return err
	}
	wire, err := marshalWire(payload)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	body, order, err := canonicalize(wire)
	if err != nil {
		return fmt.Errorf("canonicalize: %w", err)
	}
	for name, content := range map[string]string{
		sc.Name + ".raw.json":  string(wire) + "\n",
		sc.Name + ".body.json": body,
		sc.Name + ".order.txt": order,
	} {
		if err := os.WriteFile(filepath.Join(outDir, name), []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "go side failed:", err)
		os.Exit(1)
	}
}
