package protocol

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Validator performs runtime JSON Schema validation of protocol messages against
// the schemas in spec/. It is a thin, optional layer: the wire types already
// round-trip, but a Validator lets a client or test assert that an instance
// conforms to its declared schema (catching drift between implementations).
//
// Construct one with NewValidator, pointing at the spec/ directory.
type Validator struct {
	specDir  string
	compiler *jsonschema.Compiler

	mu       sync.Mutex
	compiled map[string]*jsonschema.Schema
}

// NewValidator loads every schema under specDir and returns a Validator. The spec
// schemas use only internal $ref (#/$defs/...), so no network access is needed.
func NewValidator(specDir string) (*Validator, error) {
	c := jsonschema.NewCompiler()

	// Walk the spec tree and register each schema under a stable resource URL keyed
	// on its repo-relative path (e.g. "actions/create-conversation-session.schema.json"),
	// so $schema_ref strings from fixtures resolve directly.
	err := filepath.WalkDir(specDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".schema.json") {
			return nil
		}
		rel, rerr := filepath.Rel(specDir, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		f, oerr := os.Open(path)
		if oerr != nil {
			return oerr
		}
		defer f.Close()
		doc, derr := jsonschema.UnmarshalJSON(f)
		if derr != nil {
			return fmt.Errorf("parse %s: %w", rel, derr)
		}
		if aerr := c.AddResource(specURL(rel), doc); aerr != nil {
			return fmt.Errorf("add %s: %w", rel, aerr)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &Validator{
		specDir:  specDir,
		compiler: c,
		compiled: make(map[string]*jsonschema.Schema),
	}, nil
}

// specURL maps a repo-relative spec path to the in-memory resource URL used during
// compilation.
func specURL(relPath string) string {
	return "mem://spec/" + relPath
}

// schemaFor compiles (and caches) the schema referenced by a $schema_ref of the
// form "actions/foo.schema.json" or "actions/foo.schema.json#/$defs/Request".
func (v *Validator) schemaFor(ref string) (*jsonschema.Schema, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if s, ok := v.compiled[ref]; ok {
		return s, nil
	}

	path := ref
	fragment := ""
	if i := strings.IndexByte(ref, '#'); i >= 0 {
		path = ref[:i]
		fragment = ref[i+1:]
	}
	url := specURL(path)
	if fragment != "" {
		url += "#" + fragment
	}
	s, err := v.compiler.Compile(url)
	if err != nil {
		return nil, err
	}
	v.compiled[ref] = s
	return s, nil
}

// ValidateRef validates instance against the schema named by ref (a $schema_ref).
// instance must be a value decoded from JSON (map[string]any, []any, etc.).
func (v *Validator) ValidateRef(ref string, instance any) error {
	s, err := v.schemaFor(ref)
	if err != nil {
		return err
	}
	return s.Validate(instance)
}

// ValidateActionRef resolves an action's Request schema (actions/<file>#/$defs/Request)
// and validates instance against it.
func (v *Validator) ValidateActionRef(file string, instance any) error {
	return v.ValidateRef("actions/"+file+"#/$defs/Request", instance)
}

// ValidateEventRef validates instance against an event schema (events/<file>).
func (v *Validator) ValidateEventRef(file string, instance any) error {
	return v.ValidateRef("events/"+file, instance)
}
