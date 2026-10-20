package tools

import (
	"context"
	"regexp"
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
)

var (
	unicodeSpaces = regexp.MustCompile("[  -   　]")
	amPMDot       = regexp.MustCompile(`(?i) (AM|PM)\.`)
)

const narrowNoBreakSpace = " "

// normalizeToolPath replaces Unicode spaces with spaces and drops a leading
// "@", which models copy from mentions.
func normalizeToolPath(path string) string {
	normalized := unicodeSpaces.ReplaceAllString(path, " ")
	return strings.TrimPrefix(normalized, "@")
}

// resolveToolPath is the absolute path a tool's path argument names.
func resolveToolPath(ctx context.Context, e env.ExecutionEnv, path string) (string, error) {
	return e.AbsolutePath(ctx, normalizeToolPath(path))
}

// resolveReadToolPath is resolveToolPath, or the first existing variant of
// it a screenshot name or a copied path may need: a narrow no-break space
// before AM/PM, NFD, and a typographic apostrophe.
func resolveReadToolPath(ctx context.Context, e env.ExecutionEnv, path string) (string, error) {
	resolved, err := resolveToolPath(ctx, e, path)
	if err != nil {
		return "", err
	}
	nfd := norm.NFD.String(resolved)
	variants := []string{
		resolved,
		amPMDot.ReplaceAllString(resolved, narrowNoBreakSpace+"$1."),
		nfd,
		strings.ReplaceAll(resolved, "'", "’"),
		strings.ReplaceAll(nfd, "'", "’"),
	}
	seen := map[string]bool{}
	for _, variant := range variants {
		if seen[variant] {
			continue
		}
		seen[variant] = true
		exists, err := e.Exists(ctx, variant)
		if err != nil {
			return "", err
		}
		if exists {
			return variant, nil
		}
	}
	return resolved, nil
}
