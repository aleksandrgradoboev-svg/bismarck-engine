//go:build !windows

package env

func isNotDirectory(error) bool { return false }
