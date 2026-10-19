package sqlmap

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/compiler/protogen"
)

func TestParseEnabled(t *testing.T) {
	defaults := map[string]bool{"schema": true, "scanner": true, "query": true, "filter": false, "repo": false}

	for _, tc := range []struct {
		name  string
		param string
		want  map[string]bool
	}{
		{"absent enables all but opt-in", "paths=source_relative", defaults},
		{"empty enables all but opt-in", "", defaults},
		{"opt-in when named", "plugins=schema+filter", map[string]bool{"schema": true, "filter": true}},
		{"single", "plugins=schema", map[string]bool{"schema": true}},
		{"several", "plugins=schema+scanner", map[string]bool{"schema": true, "scanner": true}},
		{"alongside other params", "paths=source_relative,plugins=schema", map[string]bool{"schema": true}},
		{"spaces tolerated", "plugins= schema + scanner ", map[string]bool{"schema": true, "scanner": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseEnabled(tc.param)
			if err != nil {
				t.Fatalf("parseEnabled(%q): %v", tc.param, err)
			}
			for name := range defaults {
				if got[name] != tc.want[name] {
					t.Errorf("%q: plugin %q enabled = %v, want %v", tc.param, name, got[name], tc.want[name])
				}
			}
		})
	}
}

func TestParseEnabled_UnknownPlugin(t *testing.T) {
	_, err := parseEnabled("plugins=schema+nope")
	if err == nil {
		t.Fatal("expected an error for an unknown plugin name, got nil")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error should name the offending plugin, got: %v", err)
	}
}

// The query writer's output calls the Result type the scanner writer declares,
// so enabling it alone would generate a file that cannot compile. That has to
// fail loudly at generation time rather than produce broken Go.
func TestNew_QueryRequiresScanner(t *testing.T) {
	_, err := New(protogen.Options{}, buildRequest(t, "eager.proto", "plugins=query"))
	if err == nil {
		t.Fatal("expected an error when query is enabled without scanner, got nil")
	}
	if !strings.Contains(err.Error(), "scanner") {
		t.Errorf("error should name the missing dependency, got: %v", err)
	}
}

// Disabling a plugin must actually drop its output from the generated file.
func TestGenerate_PluginsCanBeDisabled(t *testing.T) {
	full := generate(t, "eager.proto")["eager.sqlmap.go"]
	if !strings.Contains(full, "LoadAuthorRows") || !strings.Contains(full, "AuthorTable") {
		t.Fatal("the default output should contain both query and schema code")
	}

	schemaOnly := generateWith(t, "eager.proto", "plugins=schema")["eager.sqlmap.go"]
	if strings.Contains(schemaOnly, "LoadAuthorRows") {
		t.Error("plugins=schema still emitted query code")
	}
	if strings.Contains(schemaOnly, "AuthorResult") {
		t.Error("plugins=schema still emitted scanner code")
	}
	if !strings.Contains(schemaOnly, "AuthorTable") {
		t.Error("plugins=schema dropped the schema code it was asked for")
	}
}

// filter's output imports filtrify, so existing users who omit plugins= must
// not suddenly need it; naming it is enough, it depends on no other plugin.
func TestGenerate_FilterIsOptIn(t *testing.T) {
	if full := generate(t, "eager.proto")["eager.sqlmap.go"]; strings.Contains(full, "filtrify") {
		t.Error("the default output imports filtrify")
	}
	only := generateWith(t, "eager.proto", "plugins=filter")["eager.sqlmap.go"]
	if !strings.Contains(only, "AuthorFilter") || !strings.Contains(only, "github.com/snaerverk/filtrify/pkg/filter/sql") {
		t.Error("plugins=filter emitted no filter schema")
	}
	if strings.Contains(only, "AuthorTable") || strings.Contains(only, "LoadAuthorRows") {
		t.Error("plugins=filter emitted other plugins' code")
	}
}
