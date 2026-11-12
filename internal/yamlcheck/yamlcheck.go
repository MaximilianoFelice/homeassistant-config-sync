// Package yamlcheck validates the syntax of what is about to be written to
// the box before touching the disk (design.md §7, step 1). It is a cheap
// check that does not need Home Assistant: it does not validate the schema,
// only that the file parses.
package yamlcheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Check validates the content according to repoPath's extension. Files that
// are neither YAML nor JSON pass without being inspected.
func Check(repoPath string, content []byte) error {
	switch strings.ToLower(path.Ext(repoPath)) {
	case ".yaml", ".yml":
		return checkYAML(content)
	case ".json":
		return checkJSON(content)
	}
	return nil
}

// checkYAML parses every document into a yaml.Node, which accepts any tag:
// HA's tags (!secret, !include, !include_dir_*, !input, !env_var) are not
// errors.
func checkYAML(content []byte) error {
	dec := yaml.NewDecoder(bytes.NewReader(content))
	for {
		var n yaml.Node
		err := dec.Decode(&n)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func checkJSON(content []byte) error {
	var v any
	if err := json.Unmarshal(content, &v); err != nil {
		var se *json.SyntaxError
		if errors.As(err, &se) {
			line := 1 + bytes.Count(content[:min(int(se.Offset), len(content))], []byte{'\n'})
			return fmt.Errorf("json: line %d: %w", line, se)
		}
		return err
	}
	return nil
}
