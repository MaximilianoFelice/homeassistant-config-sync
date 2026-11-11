// Package yamlcheck valida la sintaxis de lo que se va a escribir en el box
// antes de tocar el disco (design.md §7, paso 1). Es un chequeo barato que
// no necesita a Home Assistant: no valida el esquema, solo que el archivo se
// pueda parsear.
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

// Check valida el contenido según la extensión de repoPath. Los archivos que
// no son YAML ni JSON pasan sin mirar.
func Check(repoPath string, content []byte) error {
	switch strings.ToLower(path.Ext(repoPath)) {
	case ".yaml", ".yml":
		return checkYAML(content)
	case ".json":
		return checkJSON(content)
	}
	return nil
}

// checkYAML parsea todos los documentos a yaml.Node, que acepta cualquier
// tag: los de HA (!secret, !include, !include_dir_*, !input, !env_var) no
// son errores.
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
			return fmt.Errorf("json: línea %d: %w", line, se)
		}
		return err
	}
	return nil
}
