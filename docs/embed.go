// Package docs embeds the OpenAPI spec so the server can serve it at /docs/openapi.yaml.
package docs

import _ "embed"

//go:embed openapi.yaml
var OpenAPI []byte
