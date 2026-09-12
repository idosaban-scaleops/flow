package gitx

import (
	"net/url"
	"strings"
)

// ParseGitHubRemote extracts owner and repository from an origin URL. It
// handles the four shapes git remotes actually come in:
//
//	git@github.com:owner/repo.git
//	ssh://git@github.com/owner/repo.git
//	https://github.com/owner/repo.git
//
// and each of those without the .git suffix. A non-GitHub remote is not an
// error — it simply reports false, and GitHub-dependent features degrade.
func ParseGitHubRemote(raw string) (owner, name string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", false
	}

	host, path, ok := splitRemote(raw)
	if !ok || !isGitHubHost(host) {
		return "", "", false
	}

	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func splitRemote(raw string) (host, path string, ok bool) {
	// scp-like syntax: [user@]host:path, which is not a valid URL.
	if !strings.Contains(raw, "://") {
		at := strings.LastIndex(raw, "@")
		rest := raw[at+1:]
		h, p, found := strings.Cut(rest, ":")
		if !found {
			return "", "", false
		}
		return h, p, true
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", "", false
	}
	return u.Hostname(), u.Path, true
}

func isGitHubHost(host string) bool {
	host = strings.ToLower(host)
	return host == "github.com" || strings.HasSuffix(host, ".github.com")
}
