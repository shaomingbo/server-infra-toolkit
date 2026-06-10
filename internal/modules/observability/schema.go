package observability

// schema.go embeds the inbound JSON Schemas into the binary and compiles them
// ONCE at package init (fail-closed: a deleted or malformed schema makes the
// binary fail to build/start, which is stronger than a runtime read-from-disk
// that could silently degrade to "no validation").
//
// Two compiled schemas back the handler:
//   - batchSchema pins ONLY the request-body ARRAY shape (a bare JSON array,
//     minItems 1, maxItems 500). It does NOT validate per-event fields.
//   - eventSchema is the per-event shape; the handler validates each element
//     against it individually to locate which events are invalid and report the
//     rejected count (FR10/AC4) — which a single array-level $ref failure could
//     not surface.
//
// Splitting array-level from per-event validation is deliberate: it lets the
// handler COUNT rejected events instead of collapsing the whole batch into one
// failure. Both schemas are compiled from the same embedded contract files.

import (
	"bytes"
	"embed"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// contractFS embeds the inbound schema files. go:embed compiles them into the
// binary, so deleting contract/*.json turns the build red (fail-closed) instead
// of degrading validation at runtime.
//
//go:embed contract/event.schema.json contract/batch.schema.json
var contractFS embed.FS

// Resource URLs the schemas are registered under. They MUST match each file's
// $id. The two schemas are independent documents (no cross-file $ref), but each
// is registered under its canonical $id and compiled from that id.
const (
	eventSchemaID = "https://github.com/shaomingbo/server-infra-toolkit/internal/modules/observability/contract/event.schema.json"
	batchSchemaID = "https://github.com/shaomingbo/server-infra-toolkit/internal/modules/observability/contract/batch.schema.json"
)

// batchSchema validates the request-body ARRAY shape only. eventSchema validates
// a single event in isolation. Both are compiled once at init; a compile failure
// panics (fail-closed: an unusable schema must not let the process come up
// serving unvalidated writes).
var (
	batchSchema *jsonschema.Schema
	eventSchema *jsonschema.Schema
)

func init() {
	c := jsonschema.NewCompiler()

	// Register both embedded documents under their $id and compile each from that
	// id, using no network/disk loader (the documents are embedded).
	addEmbeddedResource(c, eventSchemaID, "contract/event.schema.json")
	addEmbeddedResource(c, batchSchemaID, "contract/batch.schema.json")

	var err error
	if eventSchema, err = c.Compile(eventSchemaID); err != nil {
		panic(fmt.Sprintf("observability: compile event schema: %v", err))
	}
	if batchSchema, err = c.Compile(batchSchemaID); err != nil {
		panic(fmt.Sprintf("observability: compile batch schema: %v", err))
	}
}

// addEmbeddedResource reads an embedded schema file, decodes it with the
// library's precision-preserving helper (json.Number, so integer bounds are not
// rounded through float64), and registers it under url. Any failure panics: an
// unreadable/undecodable embedded schema is a build-or-startup error, never a
// silent skip.
func addEmbeddedResource(c *jsonschema.Compiler, url, path string) {
	raw, err := contractFS.ReadFile(path)
	if err != nil {
		panic(fmt.Sprintf("observability: read embedded schema %s: %v", path, err))
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		panic(fmt.Sprintf("observability: decode embedded schema %s: %v", path, err))
	}
	if err := c.AddResource(url, doc); err != nil {
		panic(fmt.Sprintf("observability: add embedded schema %s: %v", path, err))
	}
}
