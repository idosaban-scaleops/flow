package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// SetCluster writes a clusters.{context} block into the config file, creating
// the file if necessary. The edit is surgical — it walks the YAML node tree and
// replaces (or appends) one mapping entry — so the comments that `flow config
// init` wrote survive. The write itself is atomic: temp file, fsync, rename.
func SetCluster(path, context string, cc ClusterConfig) error {
	doc, err := loadDocument(path)
	if err != nil {
		return err
	}

	clusters := ensureMapping(doc.root(), "clusters")
	value, err := toNode(cc)
	if err != nil {
		return fmt.Errorf("encode cluster %q: %w", context, err)
	}
	setMapEntry(clusters, context, value)

	return writeDocument(path, doc)
}

// SetRepoHelmRepo records the helm repo learned for a source repository.
func SetRepoHelmRepo(path, repoKey, helmRepo, chartName string) error {
	doc, err := loadDocument(path)
	if err != nil {
		return err
	}

	repos := ensureMapping(doc.root(), "repos")
	entry := mapEntry(repos, repoKey)
	if entry == nil {
		entry = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setMapEntry(repos, repoKey, entry)
	}
	setMapEntry(entry, "helm_repo", scalar(helmRepo))
	if chartName != "" {
		setMapEntry(entry, "chart_name", scalar(chartName))
	}

	return writeDocument(path, doc)
}

// document wraps a parsed YAML file. The document node matters: yaml.v3 attaches
// a file's leading comment block to it, not to the root mapping, so encoding the
// mapping alone silently drops the header that `flow config init` wrote.
type document struct{ node *yaml.Node }

func (d document) root() *yaml.Node { return d.node.Content[0] }

func newDocument() document {
	return document{node: &yaml.Node{
		Kind:    yaml.DocumentNode,
		Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}},
	}}
}

// loadDocument parses path, or returns a fresh empty document when the file
// does not exist yet.
func loadDocument(path string) (document, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path comes from the user's own config flag
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return newDocument(), nil
	case err != nil:
		return document{}, fmt.Errorf("read %s: %w", path, err)
	case len(bytes.TrimSpace(data)) == 0:
		return newDocument(), nil
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return document{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return newDocument(), nil
	}
	if doc.Content[0].Kind != yaml.MappingNode {
		return document{}, fmt.Errorf("%s: expected a YAML mapping at the top level", path)
	}
	return document{node: &doc}, nil
}

func writeDocument(path string, doc document) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc.node); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	return WriteFileAtomic(path, buf.Bytes(), 0o600)
}

// WriteFileAtomic writes data to path via a temp file in the same directory,
// fsyncs it, and renames it into place, so a crash mid-write cannot truncate
// the existing file.
//
// The parent directory is deliberately not fsynced after the rename. That
// would be needed to guarantee the rename itself survives a power loss; for a
// CLI writing its own config and registry, the atomic-replace guarantee is
// what matters and the extra syscall on every write is not worth it.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmpName, path, err)
	}
	return nil
}

// --- yaml.Node helpers -----------------------------------------------------

func scalar(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}

func toNode(v any) (*yaml.Node, error) {
	var n yaml.Node
	if err := n.Encode(v); err != nil {
		return nil, err
	}
	return &n, nil
}

// mapEntry returns the value node for key, or nil.
func mapEntry(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// setMapEntry replaces the value for key, preserving its position and the
// comments attached to the key, or appends a new entry.
func setMapEntry(m *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = value
			return
		}
	}
	m.Content = append(m.Content, scalar(key), value)
}

// ensureMapping returns the mapping stored at key, creating it when absent or
// when the existing value is null (a bare `clusters:` line with nothing under
// it, which is what an untouched `flow config init` file contains).
func ensureMapping(root *yaml.Node, key string) *yaml.Node {
	existing := mapEntry(root, key)
	if existing != nil && existing.Kind == yaml.MappingNode {
		return existing
	}
	created := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	setMapEntry(root, key, created)
	return created
}
