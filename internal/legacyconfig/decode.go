// SPDX-License-Identifier: BSD-3-Clause

// Package legacyconfig compiles the pinned routerd resource format into native
// routerd2 intent. It is deliberately a pure, rejecting migration boundary:
// unsupported configuration never produces a partially usable Document.
package legacyconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/config"
	"gopkg.in/yaml.v3"
)

// SourceRevision identifies the original schema and behavior reviewed here.
const SourceRevision = "1329ef54f296507eda79bbbc3b853783dd27251e"

// Issue contains paths and fixed explanations, never input scalar values.
// In particular, syntax/type errors do not echo passwords, keys, or tokens.
type Issue struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Line    int    `json:"line,omitempty"`
}

type UnsupportedError struct {
	Issues []Issue `json:"issues"`
}

func (e *UnsupportedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "legacy configuration cannot be migrated (%d issue(s))", len(e.Issues))
	for _, issue := range e.Issues {
		fmt.Fprintf(&b, "\n- %s [%s]: %s", issue.Path, issue.Code, issue.Message)
	}
	return b.String()
}

type rawRouter struct {
	APIVersion string         `yaml:"apiVersion"`
	Kind       string         `yaml:"kind"`
	Metadata   api.ObjectMeta `yaml:"metadata"`
	Spec       struct {
		Apply     api.ApplyPolicySpec `yaml:"reconcile"`
		Resources []rawResource       `yaml:"resources"`
	} `yaml:"spec"`
}

type rawResource struct {
	APIVersion string         `yaml:"apiVersion"`
	Kind       string         `yaml:"kind"`
	Metadata   api.ObjectMeta `yaml:"metadata"`
	Spec       yaml.Node      `yaml:"spec"`
	Status     yaml.Node      `yaml:"status"`
}

type resource struct {
	api.Resource
	node *yaml.Node
	path string
}

type decoder struct {
	issues      []Issue
	treeStopped bool
}

func (d *decoder) issue(node *yaml.Node, path, code, message string) {
	if len(d.issues) >= 512 {
		if len(d.issues) == 512 {
			d.issues = append(d.issues, Issue{Path: "$", Code: "diagnostics_truncated", Message: "more than 512 issues were found; fix these before continuing"})
		}
		return
	}
	line := 0
	if node != nil {
		line = node.Line
	}
	d.issues = append(d.issues, Issue{Path: path, Code: code, Message: message, Line: line})
}

func (d *decoder) err() error {
	if len(d.issues) == 0 {
		return nil
	}
	sort.SliceStable(d.issues, func(i, j int) bool {
		a, b := d.issues[i], d.issues[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		return a.Message < b.Message
	})
	// A shared root cause can be encountered by more than one dependent resource.
	out := make([]Issue, 0, len(d.issues))
	for _, issue := range d.issues {
		if len(out) == 0 || out[len(out)-1] != issue {
			out = append(out, issue)
		}
	}
	return &UnsupportedError{Issues: out}
}

func decode(data []byte) (rawRouter, []resource, *decoder) {
	d := &decoder{}
	var raw rawRouter
	if len(data) == 0 || len(data) > config.MaxSize {
		d.issue(nil, "$", "document_size", "expected one nonempty YAML document of at most 2 MiB")
		return raw, nil, d
	}
	stream := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := stream.Decode(&doc); err != nil {
		// yaml.TypeError and scanner errors can contain sensitive input text.
		d.issue(nil, "$", "yaml_syntax", "invalid YAML; input values are omitted from diagnostics")
		return raw, nil, d
	}
	var trailing yaml.Node
	if err := stream.Decode(&trailing); !errors.Is(err, io.EOF) {
		d.issue(nil, "$", "yaml_documents", "exactly one YAML document is required")
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		d.issue(&doc, "$", "yaml_type", "Router must be a mapping")
		return raw, nil, d
	}
	root := doc.Content[0]
	nodes := 0
	d.checkTree(root, "$", 0, &nodes)
	if len(d.issues) != 0 {
		return raw, nil, d
	}
	d.checkType(root, reflect.TypeOf(raw), "$")
	if err := root.Decode(&raw); err != nil {
		d.issue(root, "$", "yaml_type", "Router fields do not match the pinned schema")
		return raw, nil, d
	}
	if node := child(root, "spec"); node == nil || node.Kind != yaml.MappingNode {
		d.issue(node, "$.spec", "required", "an explicit Router spec mapping is required")
	}
	if raw.APIVersion != api.RouterAPIVersion {
		d.issue(child(root, "apiVersion"), "$.apiVersion", "api_version", "expected routerd.net/v1alpha1")
	}
	if raw.Kind != "Router" {
		d.issue(child(root, "kind"), "$.kind", "kind", "expected Router")
	}
	d.metadata(raw.Metadata, child(root, "metadata"), "$.metadata")
	if raw.Spec.Apply.Mode != "" && raw.Spec.Apply.Mode != "strict" {
		d.issue(child(child(root, "spec"), "reconcile"), "$.spec.reconcile.mode", "unsupported_semantics", "progressive reconciliation is not equivalent to native configuration transactions")
	}
	if len(raw.Spec.Apply.ProtectedInterfaces) != 0 || len(raw.Spec.Apply.ProtectedZones) != 0 || raw.Spec.Apply.AutoTuneConntrack {
		d.issue(child(child(root, "spec"), "reconcile"), "$.spec.reconcile", "unsupported_semantics", "adoption protection and automatic conntrack tuning require an explicit native design")
	}
	schemas := map[string]reflect.Type{}
	for _, schema := range api.ConfigResourceKinds() {
		schemas[schema.APIVersion+"/"+schema.Kind] = reflect.TypeOf(schema.Spec)
	}
	// These older, still accepted kinds are absent from ConfigResourceKinds at
	// the source pin but present in Resource.UnmarshalYAML and Linux validation.
	for _, schema := range []api.ResourceKindSchema{
		{APIVersion: api.NetAPIVersion, Kind: "Link", Spec: api.LinkSpec{}},
		{APIVersion: api.NetAPIVersion, Kind: "Bridge", Spec: api.BridgeSpec{}},
		{APIVersion: api.NetAPIVersion, Kind: "VXLANSegment", Spec: api.VXLANSegmentSpec{}},
		{APIVersion: api.NetAPIVersion, Kind: "IPv4StaticRoute", Spec: api.IPv4StaticRouteSpec{}},
		{APIVersion: api.NetAPIVersion, Kind: "IPv6StaticRoute", Spec: api.IPv6StaticRouteSpec{}},
	} {
		schemas[schema.APIVersion+"/"+schema.Kind] = reflect.TypeOf(schema.Spec)
	}
	seen := map[string]bool{}
	var resources []resource
	for i, rr := range raw.Spec.Resources {
		if i >= 4096 {
			d.issue(nil, "$.spec.resources", "resource_limit", "migration accepts at most 4096 resources")
			break
		}
		path := fmt.Sprintf("$.spec.resources[%d]", i)
		n := &rr.Spec
		d.metadata(rr.Metadata, nil, path+".metadata")
		if meaningful(&rr.Status) {
			d.issue(&rr.Status, path+".status", "observed_state", "observed status cannot be imported as desired configuration")
		}
		key := rr.APIVersion + "/" + rr.Kind
		t, ok := schemas[key]
		if !ok {
			d.issue(n, path, "unsupported_kind", "resource apiVersion/kind is not in the pinned configuration schema")
			continue
		}
		id := key + "/" + rr.Metadata.Name
		if seen[id] {
			d.issue(n, path+".metadata.name", "duplicate_resource", "resource identity must be unique")
			continue
		}
		seen[id] = true
		if n.Kind != yaml.MappingNode {
			d.issue(n, path+".spec", "yaml_type", "resource spec must be a mapping")
			continue
		}
		// Deployed firewall intent uses sourceCIDRs as well as the pinned
		// API's srcCIDRs tag. Normalize before decoding the shared Go type.
		if rr.Kind == "FirewallRule" {
			for j := 0; j < len(n.Content); j += 2 {
				if n.Content[j].Value == "sourceCIDRs" {
					n.Content[j].Value = "srcCIDRs"
				}
			}
		}
		before := len(d.issues)
		d.checkType(n, t, path+".spec")
		if len(d.issues) != before {
			continue
		}
		value := reflect.New(t)
		if err := n.Decode(value.Interface()); err != nil {
			d.issue(n, path+".spec", "yaml_type", "resource fields do not match the pinned schema")
			continue
		}
		resources = append(resources, resource{
			Resource: api.Resource{
				TypeMeta: api.TypeMeta{APIVersion: rr.APIVersion, Kind: rr.Kind},
				Metadata: rr.Metadata,
				Spec:     value.Elem().Interface(),
			},
			node: n, path: path,
		})
	}
	return raw, resources, d
}

func (d *decoder) metadata(m api.ObjectMeta, node *yaml.Node, path string) {
	if strings.TrimSpace(m.Name) == "" {
		d.issue(node, path+".name", "required", "metadata.name is required")
	}
	if len(m.Annotations) != 0 {
		d.issue(node, path+".annotations", "unsupported_metadata", "annotations are not stored by the native document; retain them outside the migration")
	}
}

func (d *decoder) checkTree(n *yaml.Node, path string, depth int, count *int) {
	if d.treeStopped {
		return
	}
	*count++
	if depth > 96 || *count > 100000 {
		d.issue(n, path, "yaml_complexity", "YAML exceeds the depth or node limit")
		d.treeStopped = true
		return
	}
	if n.Tag != "" && !strings.HasPrefix(n.Tag, "!!") {
		d.issue(n, path, "yaml_tag", "custom YAML tags must be removed before migration")
	}
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		d.issue(n, path, "yaml_alias", "anchors, aliases, and merge keys must be expanded before migration")
		return
	}
	if n.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, value := n.Content[i], n.Content[i+1]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				d.issue(key, path, "yaml_key", "mapping keys must be strings; merge keys are unsupported")
				continue
			}
			next := path + "." + safeField(key.Value)
			if seen[key.Value] {
				d.issue(key, next, "duplicate_field", "duplicate mapping key")
			}
			seen[key.Value] = true
			d.checkTree(value, next, depth+1, count)
		}
		return
	}
	for i, value := range n.Content {
		d.checkTree(value, fmt.Sprintf("%s[%d]", path, i), depth+1, count)
	}
}

func safeField(field string) string {
	if len(field) > 96 {
		return "<long-field>"
	}
	for _, r := range field {
		if r < ' ' || r == 127 {
			return "<invalid-field>"
		}
	}
	return field
}

var nodeType = reflect.TypeOf(yaml.Node{})

func (d *decoder) checkType(n *yaml.Node, t reflect.Type, path string) {
	if t == nodeType || t.Kind() == reflect.Interface || n.Tag == "!!null" {
		return
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		if n.Kind != yaml.MappingNode {
			d.issue(n, path, "yaml_type", "expected a mapping")
			return
		}
		fields := yamlFields(t)
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, value := n.Content[i], n.Content[i+1]
			// The pinned Interface decoder ignores role; zone membership supplies it.
			if t == reflect.TypeFor[api.InterfaceSpec]() && key.Value == "role" {
				continue
			}
			next := path + "." + safeField(key.Value)
			ft, ok := fields[key.Value]
			if !ok {
				d.issue(key, next, "unknown_field", "field is not in the pinned YAML schema")
				continue
			}
			d.checkType(value, ft, next)
		}
	case reflect.Slice, reflect.Array:
		if n.Kind != yaml.SequenceNode {
			d.issue(n, path, "yaml_type", "expected a sequence")
			return
		}
		for i, value := range n.Content {
			d.checkType(value, t.Elem(), fmt.Sprintf("%s[%d]", path, i))
		}
	case reflect.Map:
		if n.Kind != yaml.MappingNode {
			d.issue(n, path, "yaml_type", "expected a mapping")
			return
		}
		for i := 1; i < len(n.Content); i += 2 {
			d.checkType(n.Content[i], t.Elem(), path+"."+safeField(n.Content[i-1].Value))
		}
	default:
		if n.Kind != yaml.ScalarNode || n.Decode(reflect.New(t).Interface()) != nil {
			d.issue(n, path, "yaml_type", "field has the wrong scalar type")
		}
	}
}

func yamlFields(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		tag := f.Tag.Get("yaml")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if strings.Contains(opts, "inline") {
			for key, value := range yamlFields(f.Type) {
				out[key] = value
			}
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		out[name] = f.Type
	}
	return out
}

func child(n *yaml.Node, key string) *yaml.Node {
	if n == nil {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func meaningful(n *yaml.Node) bool {
	if n == nil || n.Kind == 0 || n.Tag == "!!null" {
		return false
	}
	if n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode {
		return len(n.Content) != 0
	}
	return n.Value != ""
}
