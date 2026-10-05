// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"

	"github.com/multigres/testkit/assert"
)

// mkField builds one field descriptor; json_name is set because the generated
// codec keys on it, and protoc always populates it in a real request.
func mkField(number int32, name, jsonName string, typ descriptorpb.FieldDescriptorProto_Type, label descriptorpb.FieldDescriptorProto_Label) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name:     proto.String(name),
		Number:   proto.Int32(number),
		JsonName: proto.String(jsonName),
		Type:     typ.Enum(),
		Label:    label.Enum(),
	}
}

// sampleRequest builds a CodeGeneratorRequest for a small proto that exercises
// every field shape the generator renders for time: a Timestamp, a Duration, a
// repeated Timestamp, a proto3 optional scalar and a nested message. The
// google/protobuf dependencies ride along as real descriptors, exactly as
// protoc sends them, so the registry must skip them and resolve their types as
// built-in kinds rather than classes.
func sampleRequest() *pluginpb.CodeGeneratorRequest {
	tsFile := &descriptorpb.FileDescriptorProto{
		Name:        proto.String("google/protobuf/timestamp.proto"),
		Package:     proto.String("google.protobuf"),
		Syntax:      proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Timestamp")}},
	}
	durFile := &descriptorpb.FileDescriptorProto{
		Name:        proto.String("google/protobuf/duration.proto"),
		Package:     proto.String("google.protobuf"),
		Syntax:      proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Duration")}},
	}

	tsField := mkField(1, "ts", "ts", descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL)
	tsField.TypeName = proto.String(".google.protobuf.Timestamp")
	dField := mkField(2, "d", "d", descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL)
	dField.TypeName = proto.String(".google.protobuf.Duration")
	tssField := mkField(3, "tss", "tss", descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, descriptorpb.FieldDescriptorProto_LABEL_REPEATED)
	tssField.TypeName = proto.String(".google.protobuf.Timestamp")

	optField := mkField(4, "opt_note", "optNote", descriptorpb.FieldDescriptorProto_TYPE_STRING, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL)
	optField.Proto3Optional = proto.Bool(true)
	optField.OneofIndex = proto.Int32(0)

	innerField := mkField(5, "inner", "inner", descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL)
	innerField.TypeName = proto.String(".test.v1.Sample.Inner")

	sample := &descriptorpb.DescriptorProto{
		Name:      proto.String("Sample"),
		Field:     []*descriptorpb.FieldDescriptorProto{tsField, dField, tssField, optField, innerField},
		OneofDecl: []*descriptorpb.OneofDescriptorProto{{Name: proto.String("_opt_note")}},
		NestedType: []*descriptorpb.DescriptorProto{{
			Name:  proto.String("Inner"),
			Field: []*descriptorpb.FieldDescriptorProto{mkField(1, "label", "label", descriptorpb.FieldDescriptorProto_TYPE_STRING, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL)},
		}},
	}
	testFile := &descriptorpb.FileDescriptorProto{
		Name:        proto.String("test/v1/sample.proto"),
		Package:     proto.String("test.v1"),
		Syntax:      proto.String("proto3"),
		Dependency:  []string{"google/protobuf/timestamp.proto", "google/protobuf/duration.proto"},
		MessageType: []*descriptorpb.DescriptorProto{sample},
	}
	return &pluginpb.CodeGeneratorRequest{
		FileToGenerate: []string{"test/v1/sample.proto"},
		ProtoFile:      []*descriptorpb.FileDescriptorProto{tsFile, durFile, testFile},
	}
}

// generatedFile returns the content of a named output, failing when absent.
func generatedFile(t *testing.T, resp *pluginpb.CodeGeneratorResponse, name string) string {
	t.Helper()
	for _, f := range resp.GetFile() {
		if f.GetName() == name {
			return f.GetContent()
		}
	}
	var have []string
	for _, f := range resp.GetFile() {
		have = append(have, f.GetName())
	}
	t.Fatalf("generated file %q not found; have %v", name, have)
	return ""
}

func TestGeneratesTimestampAndDurationFields(t *testing.T) {
	assert := assert.NewAborting(t)
	resp, err := generate(sampleRequest())
	assert.NoError(err)

	mod := generatedFile(t, resp, "sample_pb.py")
	assert.StrContains(mod, "import datetime\n")
	assert.StrContains(mod, "ts: Optional[datetime.datetime] = None")
	assert.StrContains(mod, "d: Optional[datetime.timedelta] = None")
	assert.StrContains(mod, "tss: list[datetime.datetime] = dataclasses.field(default_factory=list)")
	assert.StrContains(mod, "_ts_out(self.ts)")
	assert.StrContains(mod, "_dur_out(self.d)")
	assert.StrContains(mod, "_ts_in(_v, \"ts\")")
	assert.StrContains(mod, "def _dur_in(")
	// The emitted decoder must normalise the fraction to 6 digits before
	// fromisoformat: Python 3.9/3.10 accept only 3 or 6 fractional digits, while
	// protojson emits up to 9 (nanoseconds).
	assert.StrContains(mod, `(text[dot + 1:end] + "000000")[:6]`)
}

func TestWellKnownDependencyIsNotRendered(t *testing.T) {
	assert := assert.NewAborting(t)
	resp, err := generate(sampleRequest())
	assert.NoError(err)

	for _, f := range resp.GetFile() {
		assert.NotStrContains(f.GetName(), "timestamp_pb")
		assert.NotStrContains(f.GetName(), "duration_pb")
	}
	mod := generatedFile(t, resp, "sample_pb.py")
	assert.NotStrContains(mod, "from . import timestamp_pb")
	assert.NotStrContains(mod, "from . import duration_pb")
}

// TestNoDatetimeImportWithoutTimeFields pins the byte-identical-regeneration
// invariant: a module whose file has no Timestamp/Duration field must not gain
// an import or helpers, so regenerating an existing SDK is a no-op.
func TestNoDatetimeImportWithoutTimeFields(t *testing.T) {
	assert := assert.NewAborting(t)
	plain := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("plain/v1/plain.proto"),
		Package: proto.String("plain.v1"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name:  proto.String("Plain"),
			Field: []*descriptorpb.FieldDescriptorProto{mkField(1, "label", "label", descriptorpb.FieldDescriptorProto_TYPE_STRING, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL)},
		}},
	}
	resp, err := generate(&pluginpb.CodeGeneratorRequest{
		FileToGenerate: []string{"plain/v1/plain.proto"},
		ProtoFile:      []*descriptorpb.FileDescriptorProto{plain},
	})
	assert.NoError(err)

	mod := generatedFile(t, resp, "plain_pb.py")
	assert.NotStrContains(mod, "import datetime")
	assert.NotStrContains(mod, "_ts_out")
	assert.NotStrContains(mod, "_dur_in")
}
