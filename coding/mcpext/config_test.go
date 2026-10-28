package mcpext

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Ported from pi v1.2.0 packages/coding-agent/test/mcp-extension.test.ts,
// "MCP config". Not ported: OAuth field validation (OAuth sign-in is not
// ported; oauth is kept as written) and updateMcpServerConfig (the engine does
// not edit mcp.json).

type configPaths struct{ global, cwd string }

func setupConfig(t *testing.T, global, project string) configPaths {
	t.Helper()
	root := t.TempDir()
	cwd := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(cwd, ".pi"), 0o755); err != nil {
		t.Fatal(err)
	}
	globalPath := filepath.Join(root, "agent", "mcp.json")
	os.MkdirAll(filepath.Dir(globalPath), 0o755)
	os.WriteFile(globalPath, []byte(global), 0o644)
	os.WriteFile(filepath.Join(cwd, ".pi", "mcp.json"), []byte(project), 0o644)
	return configPaths{globalPath, cwd}
}

func load(p configPaths, trusted bool) LoadedConfig { return LoadConfig(p.global, p.cwd, trusted) }

func configJSON(t *testing.T, c ServerConfig) string {
	t.Helper()
	data, _ := json.Marshal(c)
	return string(data)
}

func checkErrors(t *testing.T, errs []string, want ...string) {
	t.Helper()
	if len(errs) != len(want) {
		t.Fatalf("errors %q, want %d", errs, len(want))
	}
	for i, w := range want {
		if !strings.Contains(errs[i], w) {
			t.Errorf("error %d %q lacks %q", i, errs[i], w)
		}
	}
}

func TestConfigMergesAndValidates(t *testing.T) {
	p := setupConfig(t, `{"mcpServers":{
		"shared":{"command":"global-cmd"},
		"remote":{"url":"https://example.com/mcp","headers":{"Authorization":"Bearer ${TOKEN}"}},
		"off":{"command":"x","enabled":false},
		"bad":{"args":["no command"]},
		"legacy":{"type":"sse","url":"https://example.com/sse"},
		"badUrl":{"url":"example.com/mcp"},
		"bad name":{"command":"x"}}}`,
		`{"mcpServers":{"shared":{"command":"project-cmd","exposure":"direct"}}}`)
	trusted := load(p, true)
	var got []string
	for _, s := range trusted.Servers {
		got = append(got, s.Name+" "+s.Scope+" "+configJSON(t, s.Config))
	}
	want := []string{
		`shared project {"command":"project-cmd","exposure":"direct"}`,
		`remote global {"url":"https://example.com/mcp","headers":{"Authorization":"Bearer ${TOKEN}"}}`,
		`off global {"command":"x","enabled":false}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("servers:\n%s", strings.Join(got, "\n"))
	}
	checkErrors(t, trusted.Errors, `server "bad" needs either "command"`, "legacy SSE transport is not supported",
		`server "badUrl": url must be an http or https URL`, `invalid server name "bad name"`)
	// Untrusted projects cannot add or override servers, since stdio servers
	// run commands.
	if c := load(p, false).Servers[0].Config; c.Command != "global-cmd" || c.Exposure != "" {
		t.Fatalf("untrusted %s", configJSON(t, c))
	}
}

func TestConfigProjectOverrides(t *testing.T) {
	p := setupConfig(t, `{"mcpServers":{"tools":{"command":"x","env":{"TOKEN":"secret"}}}}`,
		// An override cannot change the command, which would run with the
		// global env.
		`{"mcpServers":{"tools":{"enabled":false,"args":["y"]},"missing":{"enabled":false}}}`)
	c := load(p, true)
	if len(c.Servers) != 1 || c.Servers[0].Override != "" || configJSON(t, c.Servers[0].Config) != `{"command":"x","env":{"TOKEN":"secret"}}` {
		t.Fatalf("servers %+v", c.Servers)
	}
	checkErrors(t, c.Errors, `server "tools": an override can only set enabled, exposure, toolExposure`,
		`server "missing" needs "command" or "url", or a global server to override`)

	project := filepath.Join(p.cwd, ".pi", "mcp.json")
	os.WriteFile(project, []byte(`{"mcpServers":{"tools":{"enabled":false}}}`), 0o644)
	tools := load(p, true).Servers[0]
	if tools.Override != project || configJSON(t, tools.Config) != `{"command":"x","env":{"TOKEN":"secret"},"enabled":false}` {
		t.Fatalf("override %q %s", tools.Override, configJSON(t, tools.Config))
	}
}

func TestConfigRejectsNamesDifferingInDashes(t *testing.T) {
	p := setupConfig(t, `{"mcpServers":{"work-files":{"command":"a"},"work_files":{"command":"b"}}}`, `{}`)
	c := load(p, false)
	if len(c.Servers) != 1 || c.Servers[0].Name != "work-files" {
		t.Fatalf("servers %+v", c.Servers)
	}
	checkErrors(t, c.Errors, `server "work_files" conflicts with "work-files"`)
}

func TestConfigExposureAndAutoEnableCodemode(t *testing.T) {
	p := setupConfig(t, `{"autoEnableCodemode":false,"mcpServers":{
		"later":{"command":"x","exposure":"deferred"},
		"scripts":{"command":"x","exposure":"codemode-deferred","toolExposure":{"a":"codemode-deferred"}},
		"off":{"command":"x","exposure":"hidden"},
		"wrong":{"command":"x","exposure":"model-only"},
		"described":{"command":"x","description":"Docs search"},
		"badDescription":{"command":"x","description":1}}}`,
		`{"autoEnableCodemode":"yes","mcpServers":{}}`)
	untrusted := load(p, false)
	if untrusted.AutoEnableCodemode == nil || *untrusted.AutoEnableCodemode {
		t.Fatal("autoEnableCodemode")
	}
	var got []string
	for _, s := range untrusted.Servers {
		got = append(got, s.Name+"="+string(s.Config.Exposure))
	}
	if strings.Join(got, " ") != "later=deferred scripts=codemode off=hidden described=" {
		t.Fatalf("exposures %v", got)
	}
	if te := untrusted.Servers[1].Config.ToolExposure; len(te) != 1 || te[0] != (ToolExposureEntry{"a", ExposureCodemode}) {
		t.Fatalf("toolExposure %+v", te)
	}
	if untrusted.Servers[3].Config.Description != "Docs search" {
		t.Fatal("description")
	}
	checkErrors(t, untrusted.Errors, `server "wrong": exposure must be one of`, `server "badDescription": description must be a string`)
	trusted := load(p, true)
	if *trusted.AutoEnableCodemode {
		t.Fatal("trusted autoEnableCodemode")
	}
	if !strings.Contains(strings.Join(trusted.Errors, "\n"), "autoEnableCodemode must be a boolean") {
		t.Fatalf("errors %q", trusted.Errors)
	}
}

func TestToolExposureResolution(t *testing.T) {
	p := setupConfig(t, `{"mcpServers":{
		"gh":{"command":"x","exposure":"deferred","toolExposure":{"get_*":"codemode","get_me":"direct","*delete*":"hidden","get_file.*":"direct"}},
		"bad":{"command":"x","toolExposure":{"a":"visible"}}}}`, `{}`)
	c := load(p, false)
	checkErrors(t, c.Errors, `server "bad": toolExposure "a" must be one of`)
	cfg := c.Servers[0].Config
	for tool, want := range map[string]Exposure{"get_me": ExposureDirect, "get_issue": ExposureCodemode, "get_delete_hint": ExposureCodemode, "delete_repo": ExposureHidden, "list_issues": ExposureDeferred} {
		if got := ToolExposureOf(cfg, tool); got != want {
			t.Errorf("%s: %s, want %s", tool, got, want)
		}
	}
	// Only "*" is special.
	if got := ToolExposureOf(ServerConfig{Command: "x", ToolExposure: []ToolExposureEntry{{"get_file.*", ExposureDirect}}}, "get_file_x"); got != ExposureCodemode {
		t.Errorf("got %s", got)
	}
}

func TestProviderAuthOnlyGlobal(t *testing.T) {
	p := setupConfig(t, `{"mcpServers":{
		"radius":{"url":"https://radius.example/mcp","auth":{"provider":"radius"}},
		"local":{"url":"http://localhost:8788/mcp","auth":{"provider":"radius-dev"}},
		"plain":{"url":"http://radius.example/mcp","auth":{"provider":"radius"}},
		"empty":{"url":"https://radius.example/mcp","auth":{"provider":""}}}}`,
		`{"mcpServers":{"radius":{"url":"https://evil.example/mcp","auth":{"provider":"radius"}}}}`)
	c := load(p, true)
	var got []string
	for _, s := range c.Servers {
		got = append(got, fmt.Sprintf("%s %s %s", s.Name, s.Scope, s.Config.URL))
	}
	// The project entry cannot replace the global one: it would send the
	// credential to its own URL.
	if strings.Join(got, "|") != "radius global https://radius.example/mcp|local global http://localhost:8788/mcp" {
		t.Fatalf("servers %v", got)
	}
	checkErrors(t, c.Errors, `server "plain": auth requires an https URL`, `server "empty": auth.provider must be a provider name`,
		`server "radius": auth is only allowed in the global mcp.json`)
}

// pi's "reads and edits mcp.json files that start with a UTF-8 BOM"
// (bdd9f039, #11191), its reading half: the engine does not edit mcp.json.
func TestConfigReadsFilesThatStartWithABOM(t *testing.T) {
	bom := string([]byte{0xEF, 0xBB, 0xBF})
	p := setupConfig(t, bom+`{"mcpServers":{"demo":{"command":"echo","enabled":false}}}`+"\n",
		bom+`{"mcpServers":{"local":{"command":"local-cmd"}}}`+"\n")
	loaded := load(p, true)
	checkErrors(t, loaded.Errors)
	var got []string
	for _, s := range loaded.Servers {
		got = append(got, s.Name+" "+s.Scope)
	}
	if strings.Join(got, ",") != "demo global,local project" {
		t.Fatalf("servers %v", got)
	}
}
