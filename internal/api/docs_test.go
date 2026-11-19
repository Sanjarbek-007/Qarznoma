package api

// These tests keep docs/openapi.yaml in sync with the code. They need no database.

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"

	"github.com/Qarznoma/qarznoma-backend/docs"
	"github.com/Qarznoma/qarznoma-backend/internal/config"
)

type specSchema struct {
	Required   []string               `yaml:"required"`
	Properties map[string]*specSchema `yaml:"properties"`
}

type openAPI struct {
	Paths      map[string]map[string]any `yaml:"paths"`
	Components struct {
		Schemas map[string]*specSchema `yaml:"schemas"`
	} `yaml:"components"`
}

func loadSpec(t *testing.T) (openAPI, map[string]any) {
	t.Helper()
	var spec openAPI
	if err := yaml.Unmarshal(docs.OpenAPI, &spec); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(docs.OpenAPI, &raw); err != nil {
		t.Fatal(err)
	}
	return spec, raw
}

func testHandler() http.Handler {
	cfg := &config.Config{JWTSecret: []byte("test-secret-test-secret-test-secret!")}
	return New(cfg, nil).Handler()
}

var ginParam = regexp.MustCompile(`:([A-Za-z]+)`)

// Every API route is documented, and every documented operation exists.
func TestOpenAPICoversAllRoutes(t *testing.T) {
	spec, _ := loadSpec(t)
	eng := testHandler().(*gin.Engine)

	routes := map[string]bool{}
	for _, r := range eng.Routes() {
		if strings.HasPrefix(r.Path, "/docs") {
			continue
		}
		routes[strings.ToLower(r.Method)+" "+ginParam.ReplaceAllString(r.Path, "{$1}")] = true
	}
	documented := map[string]bool{}
	for path, ops := range spec.Paths {
		for method := range ops {
			if method == "parameters" {
				continue
			}
			documented[method+" "+path] = true
		}
	}

	if len(routes) < 40 || len(documented) < 40 {
		t.Fatalf("suspiciously few routes (%d) or documented operations (%d)", len(routes), len(documented))
	}

	var missing, stale []string
	for k := range routes {
		if !documented[k] {
			missing = append(missing, k)
		}
	}
	for k := range documented {
		if !routes[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("routes not in docs/openapi.yaml:\n  %s", strings.Join(missing, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("documented in docs/openapi.yaml but no such route:\n  %s", strings.Join(stale, "\n  "))
	}
}

// Sync row schemas list exactly the columns the server reads and returns.
func TestOpenAPISyncRowsMatchTables(t *testing.T) {
	spec, _ := loadSpec(t)
	schemaFor := map[string]string{
		"debtors": "DebtorRow", "debts": "DebtRow", "finance_categories": "FinanceCategoryRow",
		"finance_tags": "FinanceTagRow", "finance_goals": "FinanceGoalRow",
		"finance_transactions": "FinanceTransactionRow", "notes": "NoteRow",
	}
	if len(schemaFor) != len(syncTables) {
		t.Fatalf("syncTables has %d tables, test maps %d", len(syncTables), len(schemaFor))
	}
	for table, st := range syncTables {
		name := schemaFor[table]
		sc := spec.Components.Schemas[name]
		if sc == nil {
			t.Errorf("%s: schema %s missing", table, name)
			continue
		}
		var want, wantReq []string
		for _, c := range st.cols {
			want = append(want, c.name)
			if c.required {
				wantReq = append(wantReq, c.name)
			}
		}
		for _, c := range st.serverCols {
			want = append(want, c.name)
		}
		var got []string
		for k := range sc.Properties {
			got = append(got, k)
		}
		slices.Sort(want)
		slices.Sort(got)
		slices.Sort(wantReq)
		gotReq := slices.Clone(sc.Required)
		slices.Sort(gotReq)
		if !slices.Equal(want, got) {
			t.Errorf("%s properties:\n  code: %v\n  spec: %v", name, want, got)
		}
		if !slices.Equal(wantReq, gotReq) {
			t.Errorf("%s required:\n  code: %v\n  spec: %v", name, wantReq, gotReq)
		}
	}
}

// Every $ref points at something that exists.
func TestOpenAPIRefsResolve(t *testing.T) {
	_, raw := loadSpec(t)
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if ref, ok := x["$ref"].(string); ok {
				node := any(raw)
				for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
					m, ok := node.(map[string]any)
					if !ok || m[part] == nil {
						t.Errorf("unresolved $ref %s", ref)
						return
					}
					node = m[part]
				}
			}
			for _, vv := range x {
				walk(vv)
			}
		case []any:
			for _, vv := range x {
				walk(vv)
			}
		}
	}
	walk(raw)
}

func TestDocsServed(t *testing.T) {
	h := testHandler()
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	if w := get("/docs"); w.Code != 200 || !strings.Contains(w.Body.String(), "swagger-ui") {
		t.Errorf("/docs: %d", w.Code)
	}
	if w := get("/docs/openapi.yaml"); w.Code != 200 || !strings.HasPrefix(w.Body.String(), "openapi: 3") {
		t.Errorf("/docs/openapi.yaml: %d", w.Code)
	}

	cfg := &config.Config{JWTSecret: []byte("test-secret-test-secret-test-secret!"), DisableDocs: true}
	w := httptest.NewRecorder()
	New(cfg, nil).Handler().ServeHTTP(w, httptest.NewRequest("GET", "/docs", nil))
	if w.Code != 404 {
		t.Errorf("DISABLE_DOCS: /docs gave %d, want 404", w.Code)
	}
}
