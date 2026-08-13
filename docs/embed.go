// Package docs embeds the OpenAPI specification into the binary.
//
// The spec used to be read from disk relative to the process working directory,
// which meant /docs/openapi.yaml returned 404 in the container image (docs/ is
// never copied into the runtime layer) and only worked when the binary was run
// from the repository root. Embedding removes both failure modes.
package docs

import _ "embed"

// OpenAPISpec is the raw YAML of docs/openapi.yaml.
//
//go:embed openapi.yaml
var OpenAPISpec []byte
