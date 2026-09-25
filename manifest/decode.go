// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"mime"
	"reflect"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	v1 "latere.ai/x/cella/manifest/v1"
)

// The media types a manifest body may carry. JSON is the wire form every
// generated client sends; the three YAML types are what a person writing a
// manifest by hand sends, and the two syntaxes decode to one object.
const (
	MediaJSON     = "application/json"
	MediaYAML     = "application/yaml"
	MediaXYAML    = "application/x-yaml"
	MediaTextYAML = "text/yaml"
)

// The two bounds design 003 puts on a YAML body. AliasBudget is how many bytes
// the aliases of one document may expand to, and MaxDepth how deep it may
// nest. Both are checked on the node tree before any value is built, so a
// document that costs more to expand than it cost to send is refused rather
// than expanded.
const (
	AliasBudget = 1 << 20
	MaxDepth    = 64
)

// unmarshaler is the interface a type that decodes itself satisfies. A type
// that does decides what its own members are, so the unknown-field walk stops
// there rather than reading a document member as a Go field.
var unmarshaler = reflect.TypeFor[json.Unmarshaler]()

// decode is the one decoder design 003 gives every kind: the content types it
// names, exactly one document, the version before the kind before any field,
// unknown fields refused with their path, and the typed object last.
//
// The version and the kind are read off the generic tree and not off the
// decoded object, because a caller who sent the wrong version has to learn
// that before it learns that a field of another version's schema is unknown.
func decode(body []byte, contentType, kind string, into any) error {
	raw, err := jsonOf(body, contentType)
	if err != nil {
		return err
	}
	var tree map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&tree); err != nil {
		return fail("bad_request", err.Error())
	}
	var tail any
	if err := dec.Decode(&tail); !errors.Is(err, io.EOF) {
		return fail("multi_document", "expected exactly one manifest")
	}
	if version, _ := tree["apiVersion"].(string); version != v1.APIVersion {
		return fail("unsupported_version", "apiVersion must be "+v1.APIVersion)
	}
	if got, _ := tree["kind"].(string); got != kind {
		return fail("unsupported_kind", "kind must be "+kind)
	}
	if paths := unknownFields(tree, reflect.TypeOf(into), ""); len(paths) > 0 {
		return failPaths("unknown_field", "the schema knows no field "+paths[0], paths)
	}
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	if err := strict.Decode(into); err != nil {
		// The walk above names every member the schema does not know, so
		// what reaches here is a value of the wrong shape. The unknown-field
		// arm remains because the walk stops at a type that decodes itself,
		// and that type's own decoder may still refuse a member.
		if strings.Contains(err.Error(), "unknown field") {
			return fail("unknown_field", err.Error())
		}
		return fail("bad_request", err.Error())
	}
	return nil
}

// jsonOf is the body as JSON, whatever syntax it arrived in. A JSON body is
// itself, and a YAML body becomes the JSON its document describes; a YAML
// body that begins with an object brace is JSON, which is design 003's own
// rule and what lets a client send one syntax under either type.
func jsonOf(body []byte, contentType string) ([]byte, error) {
	media, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, fail("unsupported_media_type", "the content type could not be read: "+err.Error())
	}
	switch media {
	case MediaJSON:
		return body, nil
	case MediaYAML, MediaXYAML, MediaTextYAML:
	default:
		return nil, fail("unsupported_media_type",
			"expected "+strings.Join([]string{MediaJSON, MediaYAML, MediaXYAML, MediaTextYAML}, ", "))
	}
	if bytes.HasPrefix(bytes.TrimLeft(body, " \t\r\n"), []byte("{")) {
		return body, nil
	}
	return yamlToJSON(body)
}

// yamlToJSON reads one YAML document and renders it as JSON. One document is
// the rule: a second is multi_document, which is what a caller sending a
// stream of manifests reads.
func yamlToJSON(body []byte) ([]byte, error) {
	dec := yaml.NewDecoder(bytes.NewReader(body))
	var first yaml.Node
	if err := dec.Decode(&first); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fail("bad_request", "the body carries no manifest")
		}
		return nil, fail("bad_request", err.Error())
	}
	var second yaml.Node
	switch err := dec.Decode(&second); {
	case err == nil:
		return nil, fail("multi_document", "expected exactly one manifest")
	case !errors.Is(err, io.EOF):
		return nil, fail("bad_request", err.Error())
	}
	w := walk{left: AliasBudget}
	if err := w.expansion(&first, 0, false); err != nil {
		return nil, err
	}
	var value any
	if err := first.Decode(&value); err != nil {
		return nil, fail("bad_request", err.Error())
	}
	plain, err := jsonValue(value, "")
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(plain)
	if err != nil {
		return nil, fail("bad_request", err.Error())
	}
	return raw, nil
}

// walk is one pass over a document's node tree before any value is built.
// left is the alias budget not yet spent, and entered counts the nodes the
// pass entered, which is the work checking the document costs.
type walk struct {
	left    int
	entered int
}

// expansion charges one node against the alias budget and refuses when it is
// spent or when the document nests too deep. Only a node reached through an
// alias is charged: the budget is what expansion costs beyond what the body
// already carried, so a large document is bounded by the body cap of design
// 008 and a small one with a chain of aliases is bounded here. A node reached
// through an alias costs at least one byte and an alias leads straight to one,
// so beyond the nodes the body carries the pass enters about twice AliasBudget
// nodes at most, whatever the aliases would expand to.
func (w *walk) expansion(node *yaml.Node, depth int, aliased bool) error {
	if node == nil {
		return nil
	}
	w.entered++
	if depth > MaxDepth {
		return fail("invalid_field", "the manifest nests deeper than "+strconv.Itoa(MaxDepth)+" levels")
	}
	if w.left < 0 {
		return fail("invalid_field", "the manifest's aliases expand past "+strconv.Itoa(AliasBudget)+" bytes")
	}
	if node.Kind == yaml.AliasNode {
		return w.expansion(node.Alias, depth+1, true)
	}
	if aliased {
		w.left -= len(node.Value) + 1
	}
	for _, child := range node.Content {
		if err := w.expansion(child, depth+1, aliased); err != nil {
			return err
		}
	}
	return nil
}

// jsonValue turns what a YAML document decoded into the value JSON describes.
// YAML admits a key of any type and JSON admits a string, so a key that is
// not one is refused at its own path rather than rendered into something the
// schema would then not know.
func jsonValue(value any, path string) (any, error) {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, member := range typed {
			converted, err := jsonValue(member, join(path, key))
			if err != nil {
				return nil, err
			}
			out[key] = converted
		}
		return out, nil
	case map[any]any:
		out := make(map[string]any, len(typed))
		for key, member := range typed {
			name, ok := key.(string)
			if !ok {
				return nil, failAt("invalid_field", path, "a field name is a string")
			}
			converted, err := jsonValue(member, join(path, name))
			if err != nil {
				return nil, err
			}
			out[name] = converted
		}
		return out, nil
	case []any:
		out := make([]any, len(typed))
		for i, member := range typed {
			converted, err := jsonValue(member, path+"["+strconv.Itoa(i)+"]")
			if err != nil {
				return nil, err
			}
			out[i] = converted
		}
		return out, nil
	default:
		return value, nil
	}
}

// join is one JSON path and the member below it.
func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// unknownFields walks one decoded document against the JSON shape of target
// and returns the path of every member the schema does not know, sorted so
// two runs over one document name them in one order. It is the path half of
// design 003's rule: encoding/json reports which field it did not know and
// never where the field sat, and a caller fixing a manifest needs where.
func unknownFields(value any, target reflect.Type, prefix string) []string {
	for target.Kind() == reflect.Pointer {
		target = target.Elem()
	}
	if target.Implements(unmarshaler) || reflect.PointerTo(target).Implements(unmarshaler) {
		return nil
	}
	switch target.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		fields := jsonFields(target)
		var out []string
		for _, name := range sortedKeys(object) {
			field, known := fields[name]
			if !known {
				out = append(out, join(prefix, name))
				continue
			}
			out = append(out, unknownFields(object[name], field, join(prefix, name))...)
		}
		return out
	case reflect.Map:
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		var out []string
		for _, name := range sortedKeys(object) {
			out = append(out, unknownFields(object[name], target.Elem(), join(prefix, name))...)
		}
		return out
	case reflect.Slice, reflect.Array:
		items, ok := value.([]any)
		if !ok {
			return nil
		}
		var out []string
		for i, item := range items {
			out = append(out, unknownFields(item, target.Elem(), prefix+"["+strconv.Itoa(i)+"]")...)
		}
		return out
	default:
		return nil
	}
}

// jsonFields is the members one struct declares, keyed by the name a document
// writes them under. A field the tag hides is left out, so a document naming
// one is unknown, which is what the strict decoder would say of it too. An
// embedded struct's fields are the outer struct's, as JSON reads them.
func jsonFields(target reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for field := range target.Fields() {
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if field.Anonymous && name == "" {
			embedded := field.Type
			for embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if embedded.Kind() == reflect.Struct {
				maps.Copy(out, jsonFields(embedded))
				continue
			}
		}
		if name == "" {
			name = field.Name
		}
		out[name] = field.Type
	}
	return out
}
