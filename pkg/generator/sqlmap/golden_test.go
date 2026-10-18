package sqlmap

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

// Set -update to (re)write the golden files from the current generator output.
var update = flag.Bool("update", false, "update golden files")

// generate compiles protoFile (relative to testdata/) with protoc into a
// FileDescriptorSet, feeds it through the generator exactly like
// cmd/protoc-gen-go-sqlmap does, and returns the generated files keyed by
// name.
func generate(t *testing.T, protoFile string) map[string]string {
	return generateWith(t, protoFile, "")
}

// buildRequest compiles protoFile with protoc, appending extra to the param.
func buildRequest(t *testing.T, protoFile, extra string) *pluginpb.CodeGeneratorRequest {
	t.Helper()
	if _, err := exec.LookPath("protoc"); err != nil {
		t.Skip("protoc not found in PATH")
	}

	protoRoot, err := filepath.Abs(filepath.Join("..", "..", "..", "proto"))
	if err != nil {
		t.Fatal(err)
	}
	testdata, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatal(err)
	}

	descSet := filepath.Join(t.TempDir(), "descriptor_set.binpb")
	cmd := exec.Command("protoc",
		"-I", protoRoot,
		"-I", testdata,
		"--include_imports",
		"--descriptor_set_out="+descSet,
		filepath.Join(testdata, protoFile),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("protoc: %v\n%s", err, out)
	}

	raw, err := os.ReadFile(descSet)
	if err != nil {
		t.Fatal(err)
	}
	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &fds); err != nil {
		t.Fatal(err)
	}

	param := "paths=source_relative," +
		"Msqlmap/v1/sqlmap.proto=github.com/snaerverk/protoc-gen-go-sqlmap/pkg/generated/sqlmap/v1," +
		"Mschema/v1/schema.proto=github.com/snaerverk/protoc-gen-go-sqlmap/pkg/generated/schema/v1"
	if extra != "" {
		param += "," + extra
	}
	return &pluginpb.CodeGeneratorRequest{
		FileToGenerate: []string{protoFile},
		Parameter:      proto.String(param),
		ProtoFile:      fds.File,
	}
}

// generateWith runs protoFile through the generator, keyed by file name.
func generateWith(t *testing.T, protoFile, extra string) map[string]string {
	t.Helper()
	gen, err := New(protogen.Options{}, buildRequest(t, protoFile, extra))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := gen.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("generator error: %s", resp.GetError())
	}

	out := make(map[string]string, len(resp.File))
	for _, f := range resp.File {
		out[f.GetName()] = f.GetContent()
	}
	return out
}

// generateError returns the message protoFile failed generation with, failing
// the test if it succeeded -- silent success is the bug being looked for.
func generateError(t *testing.T, protoFile string) string {
	t.Helper()
	gen, err := New(protogen.Options{}, buildRequest(t, protoFile, ""))
	if err != nil {
		return err.Error()
	}
	resp, err := gen.Generate()
	if err != nil {
		return err.Error()
	}
	if resp.Error != nil {
		return resp.GetError()
	}
	t.Fatalf("expected %s to fail generation, but it succeeded", protoFile)
	return ""
}

// compareGolden compares got against testdata/<name>.golden, or rewrites it
// when -update is passed.
func compareGolden(t *testing.T, name string, got string) {
	t.Helper()
	goldenPath := filepath.Join("testdata", name+".golden")

	if *update {
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("reading golden file %s: %v (run `go test ./pkg/generator/sqlmap/... -update` to create it)", goldenPath, err)
	}
	if got != string(want) {
		t.Errorf("generated output for %q does not match %s.\nRun with -update to accept the new output if it's expected.\n--- got ---\n%s\n--- want ---\n%s", name, goldenPath, got, want)
	}
}

func TestGenerate_Simple(t *testing.T) {
	files := generate(t, "simple.proto")
	got, ok := files["simple.sqlmap.go"]
	if !ok {
		t.Fatalf("no simple.sqlmap.go in generated output, got files: %v", fileNames(files))
	}
	compareGolden(t, "simple.sqlmap.go", got)
}

func TestGenerate_ForeignKey(t *testing.T) {
	files := generate(t, "foreignkey.proto")
	got, ok := files["foreignkey.sqlmap.go"]
	if !ok {
		t.Fatalf("no foreignkey.sqlmap.go in generated output, got files: %v", fileNames(files))
	}
	compareGolden(t, "foreignkey.sqlmap.go", got)
}

func TestGenerate_Relation(t *testing.T) {
	files := generate(t, "relation.proto")
	got, ok := files["relation.sqlmap.go"]
	if !ok {
		t.Fatalf("no relation.sqlmap.go in generated output, got files: %v", fileNames(files))
	}
	compareGolden(t, "relation.sqlmap.go", got)
}

func TestGenerate_Eager(t *testing.T) {
	files := generate(t, "eager.proto")
	got, ok := files["eager.sqlmap.go"]
	if !ok {
		t.Fatalf("no eager.sqlmap.go in generated output, got files: %v", fileNames(files))
	}
	compareGolden(t, "eager.sqlmap.go", got)
}

// A subtype may carry its own surrogate key and name the linking column with
// subtype_of.fieldnames; the referenced side stays the supertype's PK.
func TestGenerate_SubtypeFieldnames(t *testing.T) {
	files := generate(t, "subtype_fieldnames.proto")
	got, ok := files["subtype_fieldnames.sqlmap.go"]
	if !ok {
		t.Fatalf("no subtype_fieldnames.sqlmap.go in generated output, got files: %v", fileNames(files))
	}
	compareGolden(t, "subtype_fieldnames.sqlmap.go", got)
}

func TestGenerate_Subtype(t *testing.T) {
	files := generate(t, "subtype.proto")
	got, ok := files["subtype.sqlmap.go"]
	if !ok {
		t.Fatalf("no subtype.sqlmap.go in generated output, got files: %v", fileNames(files))
	}
	compareGolden(t, "subtype.sqlmap.go", got)
}

func fileNames(files map[string]string) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	return names
}

// The golden tests hand the request to the generator directly, so protoc's
// feature check never runs; without the flag protoc and buf refuse every file
// using proto3 `optional`, as relation.proto does.
func TestGenerate_SupportsProto3Optional(t *testing.T) {
	gen, err := New(protogen.Options{}, buildRequest(t, "relation.proto", ""))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := gen.Generate()
	if err != nil {
		t.Fatal(err)
	}
	want := uint64(pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL)
	if resp.GetSupportedFeatures()&want == 0 {
		t.Fatalf("SupportedFeatures = %d, want FEATURE_PROTO3_OPTIONAL set", resp.GetSupportedFeatures())
	}
}
