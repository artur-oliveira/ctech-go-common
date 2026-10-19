package problem

import (
	"fmt"
	"strings"
	"sync"
)

// DefaultBaseURI is the relative base the Type* constants use.
const DefaultBaseURI = "/problems/"

// TypeDef is one registered problem type.
type TypeDef struct {
	Slug   string // "no-billing-account"
	URI    string // base URI + Slug
	Status int
	Title  string
}

// Registry is a product's catalogue of problem types under one base URI
// (e.g. "https://accounts.aoctech.app/problems/"). Products declare their
// types once at package init and build problems from them, so a type URI is
// never a string literal at a call site.
type Registry struct {
	base string
	mu   sync.RWMutex
	defs map[string]TypeDef
}

// NewRegistry returns an empty registry. An empty baseURI means DefaultBaseURI;
// a missing trailing slash is added.
func NewRegistry(baseURI string) *Registry {
	if baseURI == "" {
		baseURI = DefaultBaseURI
	}
	if !strings.HasSuffix(baseURI, "/") {
		baseURI += "/"
	}
	return &Registry{base: baseURI, defs: map[string]TypeDef{}}
}

// Register declares a type and returns its URI. Registering the same slug with
// the same status and title again is a no-op; a conflicting definition panics,
// because it is a programming error found at init, not a runtime condition.
func (r *Registry) Register(slug string, status int, title string) string {
	def := TypeDef{Slug: slug, URI: r.base + slug, Status: status, Title: title}
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.defs[slug]; ok && old != def {
		panic(fmt.Sprintf("problem: type %q registered twice with different definitions", slug))
	}
	r.defs[slug] = def
	return def.URI
}

// URI returns the full URI for a slug, registered or not.
func (r *Registry) URI(slug string) string { return r.base + slug }

// Lookup finds a registered type by its full URI.
func (r *Registry) Lookup(uri string) (TypeDef, bool) {
	slug, ok := strings.CutPrefix(uri, r.base)
	if !ok {
		return TypeDef{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.defs[slug]
	return d, ok
}

// New builds a problem from a registered slug, with Code defaulted to the slug
// in snake_case. It panics for an unregistered slug (a programming error, same
// as Register).
func (r *Registry) New(slug, detail string) *Problem {
	r.mu.RLock()
	d, ok := r.defs[slug]
	r.mu.RUnlock()
	if !ok {
		panic(fmt.Sprintf("problem: type %q is not registered", slug))
	}
	return New(d.Status, d.URI, d.Title, detail).WithCode(CodeFromType(d.URI))
}

// CodeFromType derives the default code from a type URI: the last path
// segment with "-" replaced by "_" ("/problems/not-found" -> "not_found").
func CodeFromType(typ string) string {
	return strings.ReplaceAll(typ[strings.LastIndexByte(typ, '/')+1:], "-", "_")
}
