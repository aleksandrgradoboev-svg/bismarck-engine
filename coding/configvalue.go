package coding

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Configuration values that may be shell commands, environment variable
// references, or literals (pi core/resolve-config-value.ts). They are used for
// MCP server headers and env, and for API keys.
//
//   - a value starting with "!" runs the rest as a shell command and uses its
//     trimmed stdout (cached for the process lifetime by ResolveConfigValue);
//   - otherwise "$NAME" and "${NAME}" interpolate environment variables, and
//     "$$" and "$!" escape a literal "$" and "!".

var (
	envVarName       = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	envVarNamePrefix = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*`)
)

type templatePart struct {
	env   bool
	value string // the literal, or the variable name
}

func appendLiteral(parts []templatePart, value string) []templatePart {
	if value == "" {
		return parts
	}
	if n := len(parts); n > 0 && !parts[n-1].env {
		parts[n-1].value += value
		return parts
	}
	return append(parts, templatePart{value: value})
}

func parseConfigValueTemplate(config string) []templatePart {
	var parts []templatePart
	index := 0
	for index < len(config) {
		dollar := strings.IndexByte(config[index:], '$')
		if dollar < 0 {
			parts = appendLiteral(parts, config[index:])
			break
		}
		dollar += index
		parts = appendLiteral(parts, config[index:dollar])
		var next byte
		if dollar+1 < len(config) {
			next = config[dollar+1]
		}
		switch {
		case next == '$' || next == '!':
			parts = appendLiteral(parts, string(next))
			index = dollar + 2
		case next == '{':
			end := strings.IndexByte(config[dollar+2:], '}')
			if end < 0 {
				parts = appendLiteral(parts, "$")
				index = dollar + 1
				continue
			}
			end += dollar + 2
			name := config[dollar+2 : end]
			if envVarName.MatchString(name) {
				parts = append(parts, templatePart{env: true, value: name})
			} else {
				parts = appendLiteral(parts, config[dollar:end+1])
			}
			index = end + 1
		default:
			if match := envVarNamePrefix.FindString(config[dollar+1:]); match != "" {
				parts = append(parts, templatePart{env: true, value: match})
				index = dollar + 1 + len(match)
				continue
			}
			parts = appendLiteral(parts, "$")
			index = dollar + 1
		}
	}
	return parts
}

func lookupConfigEnv(name string, env map[string]string) (string, bool) {
	if v := env[name]; v != "" {
		return v, true
	}
	if v := os.Getenv(name); v != "" {
		return v, true
	}
	return "", false
}

func resolveTemplate(parts []templatePart, env map[string]string) (string, bool) {
	var b strings.Builder
	for _, part := range parts {
		if !part.env {
			b.WriteString(part.value)
			continue
		}
		v, ok := lookupConfigEnv(part.value, env)
		if !ok {
			return "", false
		}
		b.WriteString(v)
	}
	return b.String(), true
}

func missingConfigEnvNames(config string, env map[string]string) []string {
	if strings.HasPrefix(config, "!") {
		return nil
	}
	var names []string
	for _, part := range parseConfigValueTemplate(config) {
		if !part.env || containsString(names, part.value) {
			continue
		}
		if _, ok := lookupConfigEnv(part.value, env); !ok {
			names = append(names, part.value)
		}
	}
	return names
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

var (
	commandResultMu    sync.Mutex
	commandResultCache = map[string]commandResult{}
)

type commandResult struct {
	value string
	ok    bool
}

const configCommandTimeout = 10 * time.Second

// runConfigCommand runs command with the shell: on Windows the configured
// shell (Git Bash and the like) first and cmd.exe when there is none, as pi
// does; elsewhere /bin/sh, as Node's execSync does.
func runConfigCommand(command string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), configCommandTimeout)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		if shell, args, useStdin, err := getShellConfig(); err == nil {
			if useStdin {
				cmd = exec.CommandContext(ctx, shell, args...)
				cmd.Stdin = strings.NewReader(command)
			} else {
				cmd = exec.CommandContext(ctx, shell, append(append([]string{}, args...), command)...)
			}
		} else {
			cmd = exec.CommandContext(ctx, "cmd.exe", "/d", "/s", "/c", command)
		}
	} else {
		cmd = exec.CommandContext(ctx, "/bin/sh", "-c", command)
	}
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	value := strings.TrimSpace(string(out))
	return value, value != ""
}

// ResolveConfigValue resolves a config value; ok is false when a command fails
// or prints nothing, or an environment variable it names is unset. Command
// results are cached for the process lifetime.
func ResolveConfigValue(config string, env map[string]string) (string, bool) {
	if strings.HasPrefix(config, "!") {
		commandResultMu.Lock()
		cached, hit := commandResultCache[config]
		commandResultMu.Unlock()
		if hit {
			return cached.value, cached.ok
		}
		value, ok := runConfigCommand(config[1:])
		commandResultMu.Lock()
		commandResultCache[config] = commandResult{value, ok}
		commandResultMu.Unlock()
		return value, ok
	}
	return resolveTemplate(parseConfigValueTemplate(config), env)
}

// ResolveConfigValueOrError resolves a config value without the command cache
// and names what failed (pi resolveConfigValueOrThrow); description says what
// the value is for, e.g. `MCP server "docs" header "Authorization"`.
func ResolveConfigValueOrError(config, description string, env map[string]string) (string, error) {
	if strings.HasPrefix(config, "!") {
		if value, ok := runConfigCommand(config[1:]); ok {
			return value, nil
		}
		return "", fmt.Errorf("Failed to resolve %s from shell command: %s", description, config[1:])
	}
	if value, ok := resolveTemplate(parseConfigValueTemplate(config), env); ok {
		return value, nil
	}
	switch missing := missingConfigEnvNames(config, env); len(missing) {
	case 0:
		return "", errors.New("Failed to resolve " + description)
	case 1:
		return "", fmt.Errorf("Failed to resolve %s from environment variable: %s", description, missing[0])
	default:
		return "", fmt.Errorf("Failed to resolve %s from environment variables: %s", description, strings.Join(missing, ", "))
	}
}

// ResolveHeadersOrError resolves every header value (pi
// resolveHeadersOrThrow); nil for no headers.
func ResolveHeadersOrError(headers map[string]string, description string, env map[string]string) (map[string]string, error) {
	if len(headers) == 0 {
		return nil, nil
	}
	resolved := make(map[string]string, len(headers))
	for name, value := range headers {
		v, err := ResolveConfigValueOrError(value, fmt.Sprintf("%s header %q", description, name), env)
		if err != nil {
			return nil, err
		}
		resolved[name] = v
	}
	return resolved, nil
}
