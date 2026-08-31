package openapi

import (
	"encoding/json"
	"sort"
)

// Document is a hand-rolled OpenAPI 3.1 document.
//
// Hand-rolled, and not a third-party type, for the reason warren.md §9 gives
// for every Build decision here: the shape is small, stable and standardised,
// and a library would put its own types in a signature the whole framework
// would then be unable to change. It also keeps openapi/go.mod at core and
// nothing else.
type Document struct {
	OpenAPI    string              `json:"openapi"`
	Info       Info                `json:"info"`
	Servers    []ServerInfo        `json:"servers,omitempty"`
	Paths      map[string]PathItem `json:"paths"`
	Components Components          `json:"components,omitempty"`

	refusals []Refusal
}

// Info is the document's identity.
type Info struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
}

// ServerInfo is one entry in `servers`. It is never derived — the table knows
// no scheme, port or base path, and http://localhost:8080 in every generated
// client is worse than absence.
type ServerInfo struct {
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
}

// PathItem holds the operations registered at one path.
type PathItem map[string]Operation

// Operation is one route.
type Operation struct {
	OperationID string              `json:"operationId"`
	Summary     string              `json:"summary,omitempty"`
	Parameters  []Parameter         `json:"parameters,omitempty"`
	RequestBody *RequestBody        `json:"requestBody,omitempty"`
	Responses   map[string]Response `json:"responses"`
	Security    []map[string][]any  `json:"security,omitempty"`

	// Undescribed is the refusal, on the operation itself. It is an `x-`
	// extension so the document stays valid 3.1 while still carrying the
	// admission — a reader of the JSON sees it without reading Warren's logs.
	Undescribed []string `json:"x-warren-undescribed,omitempty"`
}

// Parameter is one path or query parameter.
type Parameter struct {
	Name     string `json:"name"`
	In       string `json:"in"`
	Required bool   `json:"required,omitempty"`
	Schema   Schema `json:"schema"`
}

// RequestBody is the decoded body an operation accepts.
type RequestBody struct {
	Required bool                 `json:"required,omitempty"`
	Content  map[string]MediaType `json:"content"`
}

// Response is one status the operation answers.
type Response struct {
	Description string               `json:"description"`
	Content     map[string]MediaType `json:"content,omitempty"`
}

// MediaType carries the schema for one content type.
type MediaType struct {
	Schema Schema `json:"schema"`
}

// Components holds the reusable schemas, keyed by Go type name.
type Components struct {
	Schemas map[string]Schema `json:"schemas,omitempty"`
}

// Schema is an OpenAPI 3.1 schema. 3.1 is JSON Schema 2020-12, so `type` may
// be a list and `nullable` does not exist.
type Schema struct {
	Ref         string            `json:"$ref,omitempty"`
	Type        string            `json:"type,omitempty"`
	Format      string            `json:"format,omitempty"`
	Items       *Schema           `json:"items,omitempty"`
	Properties  map[string]Schema `json:"properties,omitempty"`
	Required    []string          `json:"required,omitempty"`
	Enum        []string          `json:"enum,omitempty"`
	MinLength   *int              `json:"minLength,omitempty"`
	MaxLength   *int              `json:"maxLength,omitempty"`
	Minimum     *float64          `json:"minimum,omitempty"`
	Maximum     *float64          `json:"maximum,omitempty"`
	Description string            `json:"description,omitempty"`
}

// Refusal is one thing the emitter could not describe.
//
// It is not an error: the route is still emitted. It is the framework saying,
// in the document and in the log, exactly where the document is thinner than
// the API — which is the difference between a client author who knows to read
// the handler and one who believes the endpoint takes no input.
type Refusal struct {
	Route  string // "POST /uploads"
	Type   string // the Go type that could not be described, or "" for none
	Reason string
}

// Refusals returns everything the emitter could not describe, in route order.
func (d Document) Refusals() []Refusal { return d.refusals }

// JSON renders the document.
//
// The output is deterministic: Go's encoding/json sorts map keys, and every
// slice is built in a sorted order by the emitter, so the same table produces
// the same bytes on every run. That is asserted by a test rather than assumed
// — a document that reorders between builds makes every diff unreadable and
// every checked-in copy churn.
//
// It is also valid YAML 1.2, which is why no YAML dependency is bought. That
// holds because the encoder escapes to ASCII-safe JSON and Go rejects invalid
// UTF-8 in strings at encode time.
func (d Document) JSON() ([]byte, error) {
	return json.MarshalIndent(d, "", "  ")
}

// sortedKeys is used wherever a map must be walked in a fixed order.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
