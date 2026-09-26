// SPDX-License-Identifier: Apache-2.0

// protoc-gen-rafikipy generates Python dataclass message types for the SDK in
// sdk/python, so `make proto` regenerates them from the same .proto sources
// the Go code comes from and the SDK can never lag the wire. It is a protoc
// plugin (CodeGeneratorRequest on stdin, CodeGeneratorResponse on stdout),
// built by the Makefile's bin/protoc-gen-rafikipy target like the other two
// plugins this repo already builds.
//
// What it emits, per requested .proto file, one module beside the others in
// sdk/python/rafiki/_gen:
//
//   - one @dataclasses.dataclass per message (nested messages become classes
//     nested in the parent), fields named snake_case exactly as proto spells
//     them; proto3 `optional` fields and oneof arms become Optional[...] =
//     None, repeated/map become list/dict.
//   - per class, explicit to_dict()/from_dict() speaking the Connect JSON
//     codec's dialect: lowerCamelCase keys, enums as their proto names,
//     64-bit integers as strings (and decoded from strings or numbers),
//     bytes as base64, zero-valued plain fields omitted, presence fields
//     emitted only when set, unknown keys ignored on decode.
//   - one small class per enum holding its value constants as plain strings,
//     so a field still decodes when a daemon sends a value this module was
//     generated before.
//   - a module-level SERVICE_<NAME> constant per service ("rafiki.v1.Control"),
//     the string the SDK's client builds request paths from.
//
// Deliberately NOT emitted: a protobuf runtime dependency, descriptors, any
// reflection. The SDK speaks Connect's JSON codec with plain dicts and these
// dataclasses; `curl --unix-socket` stays a peer way to make the same calls.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "protoc-gen-rafikipy: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	in, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	var req pluginpb.CodeGeneratorRequest
	if err := proto.Unmarshal(in, &req); err != nil {
		return err
	}
	resp, err := generate(&req)
	if err != nil {
		return err
	}
	out, err := proto.Marshal(resp)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(out)
	return err
}

func generate(req *pluginpb.CodeGeneratorRequest) (*pluginpb.CodeGeneratorResponse, error) {
	reg := buildRegistry(req.ProtoFile)
	resp := &pluginpb.CodeGeneratorResponse{
		SupportedFeatures: proto.Uint64(uint64(pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL)),
	}
	for _, name := range req.FileToGenerate {
		fd := findFile(req.ProtoFile, name)
		if fd == nil {
			return nil, fmt.Errorf("file %q named for generation is absent from the request", name)
		}
		if err := checkFile(fd, reg); err != nil {
			return nil, err
		}
		resp.File = append(resp.File, &pluginpb.CodeGeneratorResponse_File{
			Name:    proto.String(moduleName(fd.GetName()) + ".py"),
			Content: proto.String(renderModule(fd, reg)),
		})
	}
	// One package marker per run, so sdk/python/rafiki/_gen is a regular
	// package no matter which files this invocation names.
	resp.File = append(resp.File, &pluginpb.CodeGeneratorResponse_File{
		Name:    proto.String("__init__.py"),
		Content: proto.String(genInit),
	})
	return resp, nil
}

func findFile(files []*descriptorpb.FileDescriptorProto, name string) *descriptorpb.FileDescriptorProto {
	for _, f := range files {
		if f.GetName() == name {
			return f
		}
	}
	return nil
}

// moduleBase strips the .proto suffix and any directory:
// rafiki/v1/control.proto → control.
func moduleBase(protoName string) string {
	base := protoName
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	return strings.TrimSuffix(base, ".proto")
}

// moduleName is the generated module's basename for a .proto file: the file's
// base name plus the "_pb" suffix that marks a generated module (and keeps a
// module's name from colliding with any message's class name in a Python
// path, since message names are CamelCase).
func moduleName(protoName string) string {
	return moduleBase(protoName) + "_pb"
}

// ─── registry ────────────────────────────────────────────────────────────────

// classRef names where a generated class lives: the module basename (no .py)
// and the dotted Python path within it ("SpawnRequest.ScriptSpec").
type classRef struct {
	module string
	path   string
}

// enumInfo carries an enum's zero value name, which the encoding rule omits
// on the same way protojson omits a zero-valued enum field.
type enumInfo struct {
	zero string
}

// registry resolves proto type names across every file in the request.
type registry struct {
	byFQ    map[string]*classRef // messages AND enums, ".pkg.Name" → class
	enumRef map[string]*classRef // ".pkg.Enum" → its constants class
	enumVal map[string]*enumInfo // ".pkg.Enum" → zero value
	entries map[string]*entryFields
}

// entryFields holds a map-entry message's key/value field descriptors.
type entryFields struct {
	key   *descriptorpb.FieldDescriptorProto
	value *descriptorpb.FieldDescriptorProto
}

func buildRegistry(files []*descriptorpb.FileDescriptorProto) *registry {
	reg := &registry{
		byFQ:    map[string]*classRef{},
		enumRef: map[string]*classRef{},
		enumVal: map[string]*enumInfo{},
		entries: map[string]*entryFields{},
	}
	for _, fd := range files {
		mod := moduleName(fd.GetName())
		for _, m := range fd.GetMessageType() {
			reg.addMessage(mod, m, fd.GetPackage(), m.GetName())
		}
		for _, e := range fd.GetEnumType() {
			reg.addEnum(fd.GetPackage(), e, classRef{module: mod, path: e.GetName()})
		}
	}
	return reg
}

func (reg *registry) addMessage(mod string, m *descriptorpb.DescriptorProto, pkg, pyPath string) {
	fq := fqJoin(pkg, m.GetName())
	// pyPath is the FULL Python path within the module ("SpawnRequest.ScriptSpec"),
	// which is what generated code must spell to reach a nested class from a
	// method body — Python method scope sees module globals, not the defining
	// class's attributes.
	reg.byFQ[fq] = &classRef{module: mod, path: pyPath}
	for _, nested := range m.GetNestedType() {
		if nested.GetOptions().GetMapEntry() {
			var key, value *descriptorpb.FieldDescriptorProto
			for _, f := range nested.GetField() {
				switch f.GetNumber() {
				case 1:
					key = f
				case 2:
					value = f
				}
			}
			reg.entries[fq+"."+nested.GetName()] = &entryFields{key: key, value: value}
			continue
		}
		reg.addMessage(mod, nested, fq, pyPath+"."+nested.GetName())
	}
	for _, e := range m.GetEnumType() {
		reg.addEnum(fq, e, classRef{module: mod, path: pyPath + "." + e.GetName()})
	}
}

func (reg *registry) addEnum(scope string, e *descriptorpb.EnumDescriptorProto, ref classRef) {
	fq := fqJoin(scope, e.GetName())
	reg.enumRef[fq] = &classRef{module: ref.module, path: ref.path}
	reg.byFQ[fq] = &classRef{module: ref.module, path: ref.path}
	zero := ""
	for _, v := range e.GetValue() {
		if v.GetNumber() == 0 {
			zero = v.GetName()
			break
		}
	}
	// Every proto3 enum has a zero value; an enum without one fails to
	// compile, so an empty zero here would name a malformed descriptor.
	reg.enumVal[fq] = &enumInfo{zero: zero}
}

// fqJoin joins a dotted scope and a name into the leading-dot fully-qualified
// form protoc writes into FieldDescriptorProto.type_name:
// ("rafiki.v1", "Event") → ".rafiki.v1.Event", and a nested scope that
// already carries its own leading dot keeps exactly one.
func fqJoin(scope, name string) string {
	scope = strings.TrimPrefix(scope, ".")
	if scope == "" {
		return "." + name
	}
	return "." + scope + "." + name
}

// checkFile verifies every message/enum reference in the file resolves; a
// missing dependency would otherwise surface as a broken generated module.
func checkFile(fd *descriptorpb.FileDescriptorProto, reg *registry) error {
	var walk func(m *descriptorpb.DescriptorProto, pkg string) error
	walk = func(m *descriptorpb.DescriptorProto, pkg string) error {
		fq := fqJoin(pkg, m.GetName())
		for _, f := range m.GetField() {
			t := f.GetType()
			if (t == descriptorpb.FieldDescriptorProto_TYPE_MESSAGE ||
				t == descriptorpb.FieldDescriptorProto_TYPE_ENUM) &&
				reg.entries[f.GetTypeName()] == nil && reg.byFQ[f.GetTypeName()] == nil {
				return fmt.Errorf("%s.%s: unresolved type %q", fq, f.GetName(), f.GetTypeName())
			}
		}
		for _, nested := range m.GetNestedType() {
			if nested.GetOptions().GetMapEntry() {
				continue
			}
			if err := walk(nested, fq); err != nil {
				return err
			}
		}
		return nil
	}
	for _, m := range fd.GetMessageType() {
		if err := walk(m, fd.GetPackage()); err != nil {
			return err
		}
	}
	return nil
}

const genInit = `"""Generated by protoc-gen-rafikipy — do not edit; run make proto."""
`
