// Package openapi emits an OpenAPI 3.1 document from the frozen route table,
// with no annotations and no hand-maintained spec file.
//
// The input is what boot step 5 already produced: transport.Table, which knows
// every route's verb, path, success status, guards, and the Go types of its
// request and response. The schemas come from the DTO's own `json:`, `param:`,
// `query:` and `validate:` tags — the same tags the transport adapter already
// binds and validates against, so the document cannot describe an API the
// service does not serve.
//
// GENERATION IS BOOT-TIME. The document is built once in an OnStart hook after
// the table is frozen, and /openapi.json serves precomputed bytes. Reflection
// over struct tags is a boot activity (AGENT.md invariant 7); nothing here
// runs on the request path.
//
// WHAT IT CANNOT DESCRIBE, IT REFUSES OUT LOUD. A route whose type carries no
// derivable schema — a raw escape-hatch route, an `any` field, a handler that
// takes no request — is EMITTED anyway, path and method, with a Refusal
// attached. A document smaller than the API is the one error a generated
// client acts on: a client generated from a document that omits POST /uploads
// does not merely lack a method for it, it asserts the endpoint is not there.
// Every refusal reaches Document.Refusals(), a boot WARN, and an
// x-warren-undescribed extension on the operation itself; openapi.Strict()
// turns the set into a boot failure.
package openapi
