package codemode

// Identifier is the identifier a script uses for a tool: characters that are
// not valid in a JavaScript identifier become `_`. "mcp__docs__search" stays as
// is, "my-tool" becomes "my_tool". Characters are counted as code points, as
// pi's for-of loop does, so one non-ASCII character becomes one `_`.
func Identifier(name string) string {
	id := make([]byte, 0, len(name))
	for _, r := range name {
		valid := r == '_' || r == '$' || ('A' <= r && r <= 'Z') || ('a' <= r && r <= 'z') ||
			(len(id) > 0 && '0' <= r && r <= '9')
		if valid {
			id = append(id, byte(r))
		} else {
			id = append(id, '_')
		}
	}
	if len(id) == 0 {
		return "_"
	}
	return string(id)
}
