package cli

import (
	"context"
	"os"
	"strings"

	"github.com/aymanbagabas/go-udiff"
)

// diffValues shows a unified diff between the local values file and what is
// actually deployed. The diff is computed in-process rather than by shelling
// out, so it works the same everywhere and needs no external tool.
func (a *App) diffValues(_ context.Context, localPath, deployed string) error {
	local := ""
	if localPath != "" {
		data, err := os.ReadFile(localPath) //nolint:gosec // the user's own configured values file
		if err != nil && !os.IsNotExist(err) {
			return Wrap(ExitFailure, "failure", err, "reading %s", localPath)
		}
		local = string(data)
	}

	unified := udiff.Unified(
		labelOr(localPath, "local values"), "deployed values",
		normalizeTrailing(local), normalizeTrailing(deployed))

	if a.JSON() {
		return a.Out.JSON(map[string]any{
			"values_file": localPath,
			"diff":        unified,
			"identical":   unified == "",
		})
	}
	if unified == "" {
		a.Out.Success("the local values file matches what is deployed")
		return nil
	}
	a.Out.Println(a.colorizeDiff(unified))
	return nil
}

func (a *App) colorizeDiff(unified string) string {
	t := a.Out.Theme
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(unified, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			b.WriteString(t.Bold.Render(line))
		case strings.HasPrefix(line, "@@"):
			b.WriteString(t.Muted.Render(line))
		case strings.HasPrefix(line, "+"):
			b.WriteString(t.Success.Render(line))
		case strings.HasPrefix(line, "-"):
			b.WriteString(t.Error.Render(line))
		default:
			b.WriteString(line)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func labelOr(path, fallback string) string {
	if path == "" {
		return fallback
	}
	return path
}

// normalizeTrailing ensures a trailing newline, so the diff does not report a
// spurious "no newline at end of file" hunk.
func normalizeTrailing(s string) string {
	if s == "" || strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}
