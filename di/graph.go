package di

import (
	"reflect"
	"slices"
	"strings"
)

// Graph is the registered object graph. Nodes are ordered by type name, so a
// rendering of it is stable between runs.
type Graph struct {
	Nodes []Node
}

// Node is one registered provider.
type Node struct {
	// Type is the type the provider satisfies — the T of [Provide].
	Type reflect.Type
	// Deps are the provider's parameter types, in declaration order.
	Deps []reflect.Type
	// Ctor is where the constructor itself is declared, and is the zero Site for
	// a value registered with [Supply].
	Ctor Site
	// Registered is where Provide, Supply, or Contribute was called.
	Registered Site
	// Kind distinguishes a single provider from a group member and a value.
	Kind Kind
}

// Graph returns the registered graph as data, for warren graph di and
// warren explain di. It is available after registration; [Container.Build] is not
// required.
//
// The returned value shares nothing with the container and is safe to hold.
func (c *Container) Graph() Graph {
	nodes := make([]Node, 0, len(c.order))

	for _, p := range c.order {
		nodes = append(nodes, Node{
			Type:       p.typ,
			Deps:       slices.Clone(p.deps),
			Ctor:       p.ctorSite,
			Registered: p.site,
			Kind:       p.kind,
		})
	}

	// Sorted by type, then by registration site, so that the members of a group
	// keep a stable relative order rather than an arbitrary one.
	slices.SortStableFunc(nodes, func(a, b Node) int {
		if byType := strings.Compare(typeName(a.Type), typeName(b.Type)); byType != 0 {
			return byType
		}

		return strings.Compare(a.Registered.String(), b.Registered.String())
	})

	return Graph{Nodes: nodes}
}
