package api_test

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/swaggo/swag"

	"nimbus/internal/api"
	"nimbus/internal/config"

	// Side-effect import: registers the generated swagger spec with the
	// swag runtime so swag.ReadDoc() returns it.
	_ "nimbus/internal/api/openapi"
)

// unannotatedRoutes is the running ledger of /api/* routes registered with
// chi but not yet covered by swaggo annotations. Each PR that annotates a
// handler should remove the relevant entries from this map. When the map
// is empty, the strict assertion below catches any new handler added
// without annotations and the test guards the spec from drift.
//
// Keep entries as `METHOD /path` with the BasePath (/api) stripped — this
// mirrors what chi.Walk reports relative to the route group.
var unannotatedRoutes = map[string]bool{
	// /api/docs/* serves SwaggerUI itself — intentionally not in the spec.
	"GET /docs":   true,
	"GET /docs/*": true,
}

// TestOpenAPISpec_ChiRoutesMatchSpec asserts every chi-registered /api/* route
// either has matching method+path entries in the generated OpenAPI spec or is
// listed in unannotatedRoutes above. As handlers get annotated, entries move
// out of the unannotated map; once it's empty, this test guards against
// silently merging a new handler without annotations.
//
// Not t.Parallel() — swag.ReadDoc() mutates package-global state on first
// call (template placeholder substitution), which races under -race when
// multiple openapi tests run in parallel. loadSwaggerDoc serializes via
// sync.Once.
func TestOpenAPISpec_ChiRoutesMatchSpec(t *testing.T) {
	router, ok := api.NewRouter(api.Deps{Config: &config.Config{}}).(*chi.Mux)
	if !ok {
		t.Fatalf("api.NewRouter returned non-*chi.Mux handler")
	}

	doc, err := loadSwaggerDoc()
	if err != nil {
		t.Fatalf("load swagger doc: %v", err)
	}

	var missing []string
	walkErr := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/api/") && route != "/api" {
			return nil
		}
		rel := strings.TrimPrefix(route, "/api")
		if rel == "" {
			rel = "/"
		}
		// Normalize trailing slash — chi reports `/keys/` for handlers
		// mounted via `r.Route("/keys")` + `r.Get("/", ...)`, but the
		// canonical REST path (and the swag @Router annotation) is `/keys`.
		if len(rel) > 1 {
			rel = strings.TrimSuffix(rel, "/")
		}
		// Normalize chi's regex-constrained params (`{op:start|shutdown|...}`)
		// down to plain `{op}` so they match swag's @Router path. The
		// regex is a chi runtime constraint, not part of the REST path.
		rel = chiRegexParamRE.ReplaceAllString(rel, "{$1}")
		key := method + " " + rel
		if unannotatedRoutes[key] {
			return nil
		}
		ops, ok := doc.Paths[rel]
		if !ok {
			missing = append(missing, key+"  (path absent from spec)")
			return nil
		}
		if _, ok := ops[strings.ToLower(method)]; !ok {
			missing = append(missing, key+"  (method absent from spec)")
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("chi.Walk: %v", walkErr)
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("the following chi routes are missing from the OpenAPI spec — annotate them or add to unannotatedRoutes:\n  %s",
			strings.Join(missing, "\n  "))
	}
}

// TestOpenAPISpec_KeysRoutesPresent is the phase-1 sanity check: the keys
// handler is the first one annotated end-to-end, so make sure all 7 routes
// landed in the spec. Phase 2 expands coverage and this test becomes
// redundant with the broader walk above.
//
// Not t.Parallel() — see TestOpenAPISpec_ChiRoutesMatchSpec for the reason.
func TestOpenAPISpec_KeysRoutesPresent(t *testing.T) {
	doc, err := loadSwaggerDoc()
	if err != nil {
		t.Fatalf("load swagger doc: %v", err)
	}

	expected := map[string][]string{
		"/keys":                  {"get", "post"},
		"/keys/{id}":             {"get", "delete"},
		"/keys/{id}/private-key": {"get", "post"},
		"/keys/{id}/default":     {"post"},
	}

	for path, methods := range expected {
		ops, ok := doc.Paths[path]
		if !ok {
			t.Errorf("expected %s in spec, not found", path)
			continue
		}
		for _, m := range methods {
			if _, ok := ops[m]; !ok {
				t.Errorf("expected %s %s in spec, not found", strings.ToUpper(m), path)
			}
		}
	}
}

// chiRegexParamRE matches chi's regex-constrained URL params and captures
// the bare param name. e.g. `{op:start|shutdown|stop|reboot}` → `op`.
var chiRegexParamRE = regexp.MustCompile(`\{([a-zA-Z_][a-zA-Z0-9_]*):[^}]+\}`)

type swaggerDoc struct {
	Paths map[string]map[string]json.RawMessage `json:"paths"`
}

// swag.ReadDoc mutates package-global state on its first call (template
// placeholder substitution), which races under -race if two tests call it
// in parallel. Cache the result behind a Once so every test sees the same
// already-substituted doc.
var (
	cachedSwaggerDocOnce sync.Once
	cachedSwaggerDoc     *swaggerDoc
	cachedSwaggerDocErr  error
)

func loadSwaggerDoc() (*swaggerDoc, error) {
	cachedSwaggerDocOnce.Do(func() {
		raw, err := swag.ReadDoc()
		if err != nil {
			cachedSwaggerDocErr = err
			return
		}
		var d swaggerDoc
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			cachedSwaggerDocErr = err
			return
		}
		cachedSwaggerDoc = &d
	})
	return cachedSwaggerDoc, cachedSwaggerDocErr
}
