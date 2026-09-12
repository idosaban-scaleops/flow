package exec

import "strings"

// safeChars are the characters that never need quoting in a POSIX shell.
const safeChars = "@%+=:,./-_"

// ShellQuote quotes a single argument for a POSIX shell.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !needsQuoting(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func needsQuoting(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune(safeChars, r):
		default:
			return true
		}
	}
	return false
}

// ShellJoin renders an argv as a copy-pasteable shell command line.
func ShellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = ShellQuote(a)
	}
	return strings.Join(quoted, " ")
}
