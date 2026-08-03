// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package compiler

import (
	"fmt"
	"net/http"
	"slices"

	"golang.org/x/exp/maps"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/client-go/rest"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	expv1alpha1 "sigs.k8s.io/krocodile/api/v1alpha1"
	krocel "sigs.k8s.io/krocodile/pkg/cel"
	"sigs.k8s.io/krocodile/pkg/cel/ast"
	"sigs.k8s.io/krocodile/pkg/compiler/dag"
	"sigs.k8s.io/krocodile/pkg/compiler/parser"
	"sigs.k8s.io/krocodile/pkg/compiler/schema"
	schemaresolver "sigs.k8s.io/krocodile/pkg/compiler/schema/resolver"
	"sigs.k8s.io/krocodile/pkg/compiler/variable"
)

// Compiler turns a v1alpha1.Graph into a compiled Program. It needs a
// SchemaResolver and a RESTMapper to type-check templates against their
// target GVK schemas.
//
// The schemaCache field is the live cached resolver under the combined
// resolver — held separately so the schema watcher can drive
// InvalidateSchema directly into it on CRD content changes. Nil when
// the Compiler is constructed via NewCompilerWithDependencies (i.e.
// tests); production callers go through NewCompiler.
type Compiler struct {
	schemaResolver resolver.SchemaResolver
	restMapper     meta.RESTMapper
	schemaCache    *schemaresolver.CachedSchemaResolver
}

// NewCompiler constructs a Compiler from a rest.Config. The supplied
// httpClient is used by the schema resolver and the discovery REST mapper.
func NewCompiler(cfg *rest.Config, httpClient *http.Client) (*Compiler, error) {
	sr, cached, err := schemaresolver.NewCombinedResolver(cfg, httpClient)
	if err != nil {
		return nil, fmt.Errorf("create schema resolver: %w", err)
	}
	rm, err := apiutil.NewDynamicRESTMapper(cfg, httpClient)
	if err != nil {
		return nil, fmt.Errorf("create REST mapper: %w", err)
	}
	c := NewCompilerWithDependencies(sr, rm)
	c.schemaCache = cached
	return c, nil
}

// NewCompilerWithDependencies builds a Compiler directly from an already-
// constructed schema resolver and REST mapper. Useful for tests and
// callers that wire these themselves (e.g. against a fake discovery
// client). InvalidateSchema is a no-op on Compilers built this way
// (there is no cached resolver to evict from).
func NewCompilerWithDependencies(sr resolver.SchemaResolver, rm meta.RESTMapper) *Compiler {
	return &Compiler{schemaResolver: sr, restMapper: rm}
}

// InvalidateSchema drops cached schema entries for the supplied
// GroupKind from the compiler's resolver cache, so the next compile
// re-fetches fresh data. The schema watcher calls this when a CRD's
// content changes. No-op when the compiler was built without a
// cached resolver (tests).
//
// Note: the REST mapper has its own internal cache. controller-
// runtime's dynamic REST mapper handles CRD changes via its standard
// refresh logic; we don't need a separate invalidation hook for it.
func (c *Compiler) InvalidateSchema(gk k8sschema.GroupKind) {
	if c.schemaCache == nil {
		return
	}
	c.schemaCache.InvalidateGroupKind(gk)
}

// Compile validates the Graph, parses every node's CEL expressions against
// the target schemas, builds the dependency DAG, and returns the compiled
// Program. The input Graph is not mutated.
func (c *Compiler) Compile(g *expv1alpha1.Graph) (*Program, error) {
	if err := validateGraph(g); err != nil {
		return nil, fmt.Errorf("invalid graph: %w", err)
	}

	graph := g.DeepCopy()
	schemaCache := schema.NewCache()
	p := parser.New(schemaCache)

	nodes := make(map[string]*Node, len(graph.Spec.Nodes))
	nodeSchemas := make(map[string]*spec.Schema, len(graph.Spec.Nodes))
	for i := range graph.Spec.Nodes {
		apiNode := &graph.Spec.Nodes[i]
		built, sch, err := c.buildNode(p, apiNode, i)
		if err != nil {
			return nil, fmt.Errorf("build node %q: %w", apiNode.ID, err)
		}
		nodes[built.ID] = built
		if sch != nil {
			nodeSchemas[built.ID] = sch
		}
	}

	if err := requireInputNode(nodes); err != nil {
		return nil, err
	}

	// Build an inspector environment that knows every node ID, plus iterator
	// variable names introduced by any node's forEach, plus the `each`
	// identifier. Inside the inspector everything is `dyn`; type checking
	// runs later with the typed environment.
	identifiers := maps.Keys(nodes)
	identifiers = append(identifiers, EachVarName)
	identifiers = append(identifiers, allIteratorNames(nodes)...)
	dedupe(&identifiers)

	inspectorEnv, err := krocel.DefaultEnvironment(krocel.WithResourceIDs(identifiers))
	if err != nil {
		return nil, fmt.Errorf("build inspector environment: %w", err)
	}
	inspector := ast.NewInspectorWithEnv(inspectorEnv, identifiers)

	dependencyGraph, err := buildDependencyGraph(nodes, inspector)
	if err != nil {
		return nil, fmt.Errorf("build dependency graph: %w", err)
	}
	topo, err := dependencyGraph.TopologicalSort()
	if err != nil {
		return nil, fmt.Errorf("topological sort: %w", err)
	}

	// Wrap collection node schemas as lists so other nodes see them as arrays.
	celSchemas := make(map[string]*spec.Schema, len(nodeSchemas))
	for id, sch := range nodeSchemas {
		if nodes[id].IsCollection() {
			celSchemas[id] = schemaCache.WrapAsList(sch)
		} else {
			celSchemas[id] = sch
		}
	}

	// Def nodes now contribute inferred schemas to celSchemas (set in
	// buildNode), so the typed env knows the shape of `${naming.prefix}`
	// down to its field type rather than treating the def as dyn.
	typedEnv, typeProvider, err := krocel.TypedEnvironmentWithProvider(celSchemas)
	if err != nil {
		return nil, fmt.Errorf("build typed CEL environment: %w", err)
	}

	bc := newBuildContext(typedEnv, typeProvider, schemaCache)
	for id, node := range nodes {
		var payloadSchema *spec.Schema
		if node.Kind == NodeKindTemplate {
			payloadSchema = nodeSchemas[id]
		}
		if err := validateAndCompileNode(bc, node, payloadSchema); err != nil {
			return nil, fmt.Errorf("compile node %q: %w", id, err)
		}
	}

	prog := &Program{
		DAG:              dependencyGraph,
		Nodes:            nodes,
		TopologicalOrder: topo,
		NodeSchemas:      celSchemas,
	}
	emitSchemaDependencies(prog)
	return prog, nil
}

// emitSchemaDependencies walks the compiled nodes and populates
// Program.RequiredGroupKinds (deduplicated) plus Program.HasDynamicGVK
// (true if any node has a CEL expression at the apiVersion or kind
// path). The schema watcher reads these to build its reverse index of
// "which Graphs care about which CRDs."
//
// Def nodes contribute nothing — they don't reference cluster schemas.
// Template/Ref/Watch nodes contribute their target GroupKind.
//
// Dynamic-GVK detection is conservative: any Variable whose Path is
// exactly "apiVersion" or "kind" flips HasDynamicGVK. Today the
// compiler rejects such templates earlier (extractGVKFromUnstructured
// returns an error), so HasDynamicGVK is always false in practice.
// When dynamic-GVK compilation lands, this same logic will fire
// without further wiring.
func emitSchemaDependencies(p *Program) {
	seen := make(map[k8sschema.GroupKind]struct{})
	for _, n := range p.Nodes {
		if n.Kind == NodeKindDef {
			continue
		}
		for _, v := range n.Variables {
			if v.Path == "apiVersion" || v.Path == "kind" {
				p.HasDynamicGVK = true
				break
			}
		}
		if n.Object == nil {
			continue
		}
		gv, err := k8sschema.ParseGroupVersion(n.Object.GetAPIVersion())
		if err != nil {
			continue
		}
		gk := k8sschema.GroupKind{Group: gv.Group, Kind: n.Object.GetKind()}
		if gk.Kind == "" {
			continue
		}
		if _, dup := seen[gk]; dup {
			continue
		}
		seen[gk] = struct{}{}
		p.RequiredGroupKinds = append(p.RequiredGroupKinds, gk)
	}
}

// buildNode produces a single compiled Node from its API form, parses CEL
// fragments out of the payload, and returns the OpenAPI schema the node
// publishes to scope (nil for Def).
func (c *Compiler) buildNode(p *parser.Parser, n *expv1alpha1.Node, order int) (*Node, *spec.Schema, error) {
	kind, payload, err := projectPayload(n)
	if err != nil {
		return nil, nil, err
	}

	// Def nodes have no target GVK, but we still infer an OpenAPI
	// schema from the literal payload so the typed CEL env can narrow
	// def-sourced expressions. Fields whose literal value is a CEL
	// fragment (e.g. `${other.x}`) stay dyn — see inferDefSchema.
	if kind == NodeKindDef {
		descriptors, _, err := parser.ParseSchemalessResource(payload)
		if err != nil {
			return nil, nil, fmt.Errorf("parse def payload: %w", err)
		}
		forEach, err := parseForEachDimensions(n.ForEach)
		if err != nil {
			return nil, nil, err
		}
		includeWhen, readyWhen, err := parseConditions(n)
		if err != nil {
			return nil, nil, err
		}
		return &Node{
			ID:          n.ID,
			Index:       order,
			Kind:        kind,
			Object:      &unstructured.Unstructured{Object: payload},
			Variables:   fieldDescriptorsToVariables(descriptors),
			ForEach:     forEach,
			IncludeWhen: includeWhen,
			ReadyWhen:   readyWhen,
		}, inferDefSchema(payload), nil
	}

	// Template/Ref/Watch all target a real GVK. Resolve schema and
	// REST mapping, then parse the payload for CEL fragments.
	// Templates are user-authored manifests so we enforce metadata-shape
	// strictly. Ref/Watch are synthesized from typed structs that don't
	// carry a metadata field — apiVersion + kind are still required.
	if err := validateKubernetesObjectStructure(payload, kind == NodeKindTemplate); err != nil {
		return nil, nil, err
	}
	gvk, err := extractGVKFromUnstructured(payload)
	if err != nil {
		return nil, nil, err
	}
	sch, err := c.schemaResolver.ResolveSchema(gvk)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve schema for %s: %w", gvk, err)
	}
	mapping, err := c.restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, nil, fmt.Errorf("REST mapping for %s: %w", gvk, err)
	}
	if mapping.Scope.Name() != meta.RESTScopeNameNamespace {
		// Cluster-scoped targets must not carry a namespace; otherwise the
		// SSA apply silently lands in the wrong shape and the user has no
		// idea why their resource didn't reach the cluster.
		if ns := nestedString(payload, "metadata", "namespace"); ns != "" {
			return nil, nil, fmt.Errorf("%s is cluster-scoped but template sets metadata.namespace=%q", gvk.Kind, ns)
		}
	}

	var descriptors []variable.FieldDescriptor
	if kind == NodeKindTemplate {
		descriptors, err = p.ParseResource(payload, sch)
	} else {
		// Ref/Watch payloads are synthesized from typed structs (ExternalRef
		// or WatchSpec). The OpenAPI schema for the target GVK does not match
		// that shape — parse schemaless instead.
		descriptors, _, err = parser.ParseSchemalessResource(payload)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("parse %s payload: %w", kind, err)
	}

	forEach, err := parseForEachDimensions(n.ForEach)
	if err != nil {
		return nil, nil, err
	}
	includeWhen, readyWhen, err := parseConditions(n)
	if err != nil {
		return nil, nil, err
	}

	return &Node{
		ID:          n.ID,
		Index:       order,
		Kind:        kind,
		GVR:         mapping.Resource,
		Namespaced:  mapping.Scope.Name() == meta.RESTScopeNameNamespace,
		Object:      &unstructured.Unstructured{Object: payload},
		Variables:   fieldDescriptorsToVariables(descriptors),
		ForEach:     forEach,
		IncludeWhen: includeWhen,
		ReadyWhen:   readyWhen,
	}, sch, nil
}

// parseConditions parses the API node's IncludeWhen and ReadyWhen string
// lists into compiled Expressions. The CEL programs are populated later
// by validateAndCompileNode.
func parseConditions(n *expv1alpha1.Node) (includeWhen, readyWhen []*krocel.Expression, err error) {
	if len(n.IncludeWhen) > 0 {
		includeWhen, err = parser.ParseConditionExpressions(n.IncludeWhen)
		if err != nil {
			return nil, nil, fmt.Errorf("includeWhen: %w", err)
		}
	}
	if len(n.ReadyWhen) > 0 {
		readyWhen, err = parser.ParseConditionExpressions(n.ReadyWhen)
		if err != nil {
			return nil, nil, fmt.Errorf("readyWhen: %w", err)
		}
	}
	return includeWhen, readyWhen, nil
}

// projectPayload converts the discriminated-union API node into a single
// unstructured map suitable for CEL extraction.
func projectPayload(n *expv1alpha1.Node) (NodeKind, map[string]interface{}, error) {
	switch {
	case n.Template != nil:
		obj, err := unmarshalRaw(n.Template.Raw)
		if err != nil {
			return 0, nil, fmt.Errorf("template: %w", err)
		}
		return NodeKindTemplate, obj, nil
	case n.Ref != nil:
		obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(n.Ref)
		if err != nil {
			return 0, nil, fmt.Errorf("ref: %w", err)
		}
		return NodeKindRef, obj, nil
	case n.Watch != nil:
		obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(n.Watch)
		if err != nil {
			return 0, nil, fmt.Errorf("watch: %w", err)
		}
		return NodeKindWatch, obj, nil
	case n.Def != nil:
		obj, err := unmarshalRaw(n.Def.Raw)
		if err != nil {
			return 0, nil, fmt.Errorf("def: %w", err)
		}
		return NodeKindDef, obj, nil
	default:
		return 0, nil, fmt.Errorf("no payload set")
	}
}

func unmarshalRaw(raw []byte) (map[string]interface{}, error) {
	if len(raw) == 0 {
		return map[string]interface{}{}, nil
	}
	out := map[string]interface{}{}
	if err := yaml.UnmarshalStrict(raw, &out); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return out, nil
}

// validateKubernetesObjectStructure ensures the payload looks like a K8s
// object: apiVersion + kind set as non-empty strings and apiVersion's
// version segment matches the Kubernetes versioning convention. When
// requireMetadata is true the payload must also carry a metadata object —
// this is true for user-authored Templates but false for Ref / Watch
// payloads which are synthesized from typed structs.
func validateKubernetesObjectStructure(obj map[string]interface{}, requireMetadata bool) error {
	if obj == nil {
		return fmt.Errorf("payload is empty")
	}
	for _, field := range []string{"apiVersion", "kind"} {
		v, ok := obj[field]
		if !ok {
			return fmt.Errorf("missing required field %q", field)
		}
		if s, ok := v.(string); !ok || s == "" {
			return fmt.Errorf("field %q must be a non-empty string", field)
		}
	}
	apiVersion, _ := obj["apiVersion"].(string)
	gv, err := k8sschema.ParseGroupVersion(apiVersion)
	if err != nil {
		return fmt.Errorf("apiVersion %q: %w", apiVersion, err)
	}
	if !kubernetesVersionRegex.MatchString(gv.Version) {
		return fmt.Errorf("apiVersion version %q is not a valid Kubernetes version (expected v1, v1alpha1, v1beta1, ...)", gv.Version)
	}
	if requireMetadata {
		md, ok := obj["metadata"]
		if !ok {
			return fmt.Errorf("missing required field \"metadata\"")
		}
		if _, ok := md.(map[string]interface{}); !ok {
			return fmt.Errorf("field \"metadata\" must be an object")
		}
	}
	return nil
}

// isIdentityFieldPath reports whether path identifies a resource's
// identity field. metadata.name is identity for every resource;
// metadata.namespace is identity only for namespaced resources.
func isIdentityFieldPath(path string, namespaced bool) bool {
	switch path {
	case "metadata.name":
		return true
	case "metadata.namespace":
		return namespaced
	}
	return false
}

// nestedString looks up a dotted path in obj and returns the string value
// at that location, or "" if any segment is missing / wrong type. Used to
// peek at metadata.namespace before kicking off the full resolver pipeline.
func nestedString(obj map[string]interface{}, path ...string) string {
	cur := any(obj)
	for _, seg := range path {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return ""
		}
		cur, ok = m[seg]
		if !ok {
			return ""
		}
	}
	s, _ := cur.(string)
	return s
}

// extractGVKFromUnstructured parses apiVersion/kind into a GVK.
func extractGVKFromUnstructured(obj map[string]interface{}) (k8sschema.GroupVersionKind, error) {
	apiVersion, _ := obj["apiVersion"].(string)
	kind, _ := obj["kind"].(string)
	gv, err := k8sschema.ParseGroupVersion(apiVersion)
	if err != nil {
		return k8sschema.GroupVersionKind{}, fmt.Errorf("parse apiVersion %q: %w", apiVersion, err)
	}
	return gv.WithKind(kind), nil
}

// fieldDescriptorsToVariables wraps every parsed CEL field as a static
// variable; the dependency pass promotes them to Dynamic/Iteration kinds.
func fieldDescriptorsToVariables(descriptors []variable.FieldDescriptor) []*variable.ResourceField {
	if len(descriptors) == 0 {
		return nil
	}
	out := make([]*variable.ResourceField, 0, len(descriptors))
	for _, fd := range descriptors {
		out = append(out, &variable.ResourceField{
			Kind:            variable.ResourceVariableKindStatic,
			FieldDescriptor: fd,
		})
	}
	return out
}

// parseForEachDimensions compiles each {name → expression} entry into a
// ForEachDimension. The expression is parsed (but not yet type-checked).
func parseForEachDimensions(dims []expv1alpha1.ForEachDimension) ([]ForEachDimension, error) {
	if len(dims) == 0 {
		return nil, nil
	}
	out := make([]ForEachDimension, 0, len(dims))
	for i, dim := range dims {
		for name, expr := range dim {
			parsed, err := parser.ParseConditionExpressions([]string{expr})
			if err != nil {
				return nil, fmt.Errorf("forEach[%d] %q: %w", i, name, err)
			}
			if len(parsed) != 1 {
				return nil, fmt.Errorf("forEach[%d] %q: expected one expression, got %d", i, name, len(parsed))
			}
			out = append(out, ForEachDimension{Name: name, Expression: parsed[0]})
		}
	}
	return out, nil
}

// allIteratorNames returns the union of iterator variable names declared by
// every node's forEach.
func allIteratorNames(nodes map[string]*Node) []string {
	var names []string
	for _, n := range nodes {
		for _, iter := range n.ForEach {
			names = append(names, iter.Name)
		}
	}
	return names
}

// dedupe removes duplicate strings in place, preserving order.
func dedupe(xs *[]string) {
	seen := make(map[string]struct{}, len(*xs))
	out := (*xs)[:0]
	for _, x := range *xs {
		if _, ok := seen[x]; ok {
			continue
		}
		seen[x] = struct{}{}
		out = append(out, x)
	}
	*xs = out
}

// buildDependencyGraph walks every compiled node, inspects each CEL
// expression for references to other node IDs (and iterator variables), and
// produces a DAG. Cycles raise an error during topological sort.
func buildDependencyGraph(nodes map[string]*Node, inspector *ast.Inspector) (*dag.DirectedAcyclicGraph[string], error) {
	g := dag.NewDirectedAcyclicGraph[string]()
	for _, n := range nodes {
		if err := g.AddVertex(n.ID, n.Index); err != nil {
			return nil, fmt.Errorf("add vertex %q: %w", n.ID, err)
		}
	}

	for _, n := range nodes {
		iteratorNames := nodeIteratorNames(n)

		// Variables: classify, collect deps, and track which iterators
		// appear in identity-field paths (metadata.name and, for
		// namespaced templates, metadata.namespace).
		identityIterators := make(map[string]struct{}, len(iteratorNames))
		for _, v := range n.Variables {
			deps, iterRefs, err := extractDependencies(inspector, v.Expression, nodes, iteratorNames)
			if err != nil {
				return nil, fmt.Errorf("node %q: variable at %q: %w", n.ID, v.Path, err)
			}
			if len(iterRefs) > 0 {
				v.Kind = variable.ResourceVariableKindIteration
			} else if len(deps) > 0 && v.Kind == variable.ResourceVariableKindStatic {
				v.Kind = variable.ResourceVariableKindDynamic
			}
			for _, d := range deps {
				addDependency(n, d)
			}
			if isIdentityFieldPath(v.Path, n.Namespaced) {
				for _, it := range iterRefs {
					identityIterators[it] = struct{}{}
				}
			}
		}

		// Every forEach iterator must appear in an identity field so each
		// rendered instance has a unique GVK+name(+namespace). Without
		// this, SSA apply rejects later instances as a conflict — kro
		// catches it at compile time so users see the issue immediately.
		if len(iteratorNames) > 0 && n.Kind == NodeKindTemplate {
			var missing []string
			for _, it := range iteratorNames {
				if _, ok := identityIterators[it]; !ok {
					missing = append(missing, it)
				}
			}
			if len(missing) > 0 {
				return nil, fmt.Errorf("node %q: every forEach iterator must appear in metadata.name (or metadata.namespace for namespaced resources) to produce unique identities; missing: %v", n.ID, missing)
			}
		}

		// forEach: iterator dimensions cannot reference each other, but they
		// may reference other nodes.
		for _, dim := range n.ForEach {
			deps, iterRefs, err := extractDependencies(inspector, dim.Expression, nodes, iteratorNames)
			if err != nil {
				return nil, fmt.Errorf("node %q: forEach %q: %w", n.ID, dim.Name, err)
			}
			if len(iterRefs) > 0 {
				return nil, fmt.Errorf("node %q: forEach %q cannot reference other iterators %v", n.ID, dim.Name, iterRefs)
			}
			for _, d := range deps {
				addDependency(n, d)
			}
		}

		// includeWhen and readyWhen contribute dependencies on upstream
		// nodes too. ReadyWhen typically references the node itself; drop
		// self-references so we don't create a self-edge in the DAG.
		for i, expr := range n.IncludeWhen {
			deps, _, err := extractDependencies(inspector, expr, nodes, nil)
			if err != nil {
				return nil, fmt.Errorf("node %q: includeWhen[%d]: %w", n.ID, i, err)
			}
			for _, d := range deps {
				if d == n.ID {
					continue
				}
				addDependency(n, d)
			}
		}
		for i, expr := range n.ReadyWhen {
			deps, _, err := extractDependencies(inspector, expr, nodes, nil)
			if err != nil {
				return nil, fmt.Errorf("node %q: readyWhen[%d]: %w", n.ID, i, err)
			}
			// readyWhen is a per-node check; it must only reference the
			// node itself. Cross-node references would create implicit
			// ordering ambiguity (does node A wait for B's readiness?
			// what if B's readyWhen references A?) so kro forbids it
			// outright. Match that contract.
			for _, d := range deps {
				if d != n.ID {
					return nil, fmt.Errorf("node %q: readyWhen[%d] (%q) may only reference the node itself, found %q", n.ID, i, expr.UserExpression(), d)
				}
			}
		}

		if err := g.AddDependencies(n.ID, n.Dependencies); err != nil {
			return nil, fmt.Errorf("node %q: register deps: %w", n.ID, err)
		}
	}
	return g, nil
}

// extractDependencies inspects a single expression, returning the IDs of
// other nodes referenced and any iterator-variable references. Unknown
// identifiers that are neither node IDs nor iterators are an error.
func extractDependencies(
	inspector *ast.Inspector,
	expr *krocel.Expression,
	nodes map[string]*Node,
	iteratorNames []string,
) (nodeDeps []string, iteratorRefs []string, err error) {
	result, err := inspector.Inspect(expr.Original)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect: %w", err)
	}

	for _, dep := range result.ResourceDependencies {
		if dep.ID == EachVarName {
			continue
		}
		if !slices.Contains(expr.References, dep.ID) {
			expr.References = append(expr.References, dep.ID)
		}
		if slices.Contains(iteratorNames, dep.ID) {
			if !slices.Contains(iteratorRefs, dep.ID) {
				iteratorRefs = append(iteratorRefs, dep.ID)
			}
			continue
		}
		if _, ok := nodes[dep.ID]; ok && !slices.Contains(nodeDeps, dep.ID) {
			nodeDeps = append(nodeDeps, dep.ID)
		}
	}

	for _, unknown := range result.UnknownResources {
		if slices.Contains(iteratorNames, unknown.ID) {
			if !slices.Contains(iteratorRefs, unknown.ID) {
				iteratorRefs = append(iteratorRefs, unknown.ID)
			}
			if !slices.Contains(expr.References, unknown.ID) {
				expr.References = append(expr.References, unknown.ID)
			}
			continue
		}
		return nil, nil, fmt.Errorf("references unknown identifier %q", unknown.ID)
	}

	if len(result.UnknownFunctions) > 0 {
		return nil, nil, fmt.Errorf("uses unknown functions: %v", result.UnknownFunctions)
	}
	return nodeDeps, iteratorRefs, nil
}

func nodeIteratorNames(n *Node) []string {
	if len(n.ForEach) == 0 {
		return nil
	}
	out := make([]string, 0, len(n.ForEach))
	for _, dim := range n.ForEach {
		out = append(out, dim.Name)
	}
	return out
}

func addDependency(n *Node, dep string) {
	if !slices.Contains(n.Dependencies, dep) {
		n.Dependencies = append(n.Dependencies, dep)
	}
}
