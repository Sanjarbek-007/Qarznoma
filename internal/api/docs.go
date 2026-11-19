package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Qarznoma/qarznoma-backend/docs"
)

// Swagger UI comes from a pinned CDN build; only the browser loads it, the server has no extra dependency.
const swaggerUIVersion = "5.17.14"

const swaggerHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Qarznoma API</title>
<link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@` + swaggerUIVersion + `/swagger-ui.css">
<style>body{margin:0;background:#fff}</style>
</head>
<body>
<div id="swagger-ui"></div>
<script src="https://unpkg.com/swagger-ui-dist@` + swaggerUIVersion + `/swagger-ui-bundle.js" crossorigin></script>
<script>
window.ui = SwaggerUIBundle({
  url: "/docs/openapi.yaml",
  dom_id: "#swagger-ui",
  deepLinking: true,
  persistAuthorization: true,
  displayRequestDuration: true,
  tryItOutEnabled: true,
  filter: true,
  docExpansion: "list",
  defaultModelsExpandDepth: 0
});
</script>
</body>
</html>`

// registerDocs serves Swagger UI at /docs and the raw spec at /docs/openapi.yaml.
func registerDocs(r *gin.Engine) {
	page := func(c *gin.Context) {
		c.Header("Cache-Control", "no-cache")
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(swaggerHTML))
	}
	r.GET("/docs", page)
	r.GET("/docs/", page)
	r.GET("/docs/openapi.yaml", func(c *gin.Context) {
		c.Header("Cache-Control", "no-cache")
		c.Data(http.StatusOK, "application/yaml; charset=utf-8", docs.OpenAPI)
	})
}
