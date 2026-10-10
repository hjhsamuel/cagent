package local

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/apperrors"
)

func TestRegistrationAndExecutableValidation(t *testing.T) {
	root := t.TempDir()
	m := fixtureTool(t, root, "valid", "echo")
	dir := filepath.Join(root, "valid")
	limits := Limits{Timeout: time.Second, MaxOutputBytes: 1024}
	for _, mutate := range []func(*Manifest){
		func(m *Manifest) { m.Name = "different" },
		func(m *Manifest) { m.Version = "not-a-version" },
		func(m *Manifest) { m.CreatedAt = "yesterday" },
		func(m *Manifest) { m.UpdatedAt = "yesterday" },
		func(m *Manifest) { m.Description = "" },
		func(m *Manifest) { m.ProtocolVersion = 2 },
		func(m *Manifest) { m.Config = []byte(`null`) },
		func(m *Manifest) { m.Config = []byte(`[]`) },
		func(m *Manifest) { m.Config = []byte(`"settings"`) },
		func(m *Manifest) { m.InputSchema = []byte(`{"type":"string"}`) },
		func(m *Manifest) { m.InputSchema = []byte(`{"type":"object","$ref":"#/missing"}`) },
		func(m *Manifest) { m.Executable = "../valid/bin/fixture" },
		func(m *Manifest) { m.Executable = "/bin/fixture" },
		func(m *Manifest) { m.Executable = `C:\fixture.exe` },
		func(m *Manifest) { m.Executable = "missing" },
		func(m *Manifest) { m.Executable = "bin" },
	} {
		bad := m
		mutate(&bad)
		writeManifest(t, dir, bad)
		if _, err := Load(context.Background(), Config{ToolsDir: root}, nil, limits); !errors.Is(err, apperrors.ErrInvalidArgument) {
			t.Fatal("bad registration accepted", bad, err)
		}
	}
	writeManifest(t, dir, m)
	for _, names := range [][]string{{"unknown"}, {"valid", "valid"}, {"../valid"}, {""}} {
		if _, err := Load(context.Background(), Config{ToolsDir: root}, names, limits); !errors.Is(err, apperrors.ErrInvalidArgument) {
			t.Fatal("bad selection accepted", names, err)
		}
	}
	if _, err := Load(context.Background(), Config{ToolsDir: root, Config: map[string]json.RawMessage{"valid": []byte(`null`)}}, nil, limits); !errors.Is(err, apperrors.ErrInvalidArgument) {
		t.Fatal("null config accepted", err)
	}
	if err := os.Mkdir(filepath.Join(root, "broken"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(context.Background(), Config{ToolsDir: root}, []string{"valid"}, limits); err != nil {
		t.Fatal("disabled directory was inspected", err)
	}
	if _, err := Load(context.Background(), Config{ToolsDir: root}, nil, limits); !errors.Is(err, apperrors.ErrInvalidArgument) {
		t.Fatal("missing tool.json accepted", err)
	}
	for _, content := range []string{"# no metadata", "```json\n{}\n```\n# old format", `{}`, `null`, `{} {}`, strings.Repeat("x", 256<<10+1)} {
		if err := os.WriteFile(filepath.Join(dir, "tool.json"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(context.Background(), Config{ToolsDir: root}, []string{"valid"}, limits); !errors.Is(err, apperrors.ErrInvalidArgument) {
			t.Fatal("bad registration format accepted", err)
		}
	}
}

func TestPlainJSONRegistrationAndOptionalMaintenanceMetadata(t *testing.T) {
	minimal := []byte(`{"name":"echo","description":"返回文本","executable":"bin/echo","input_schema":{"type":"object"}}`)
	m, err := parseManifest(minimal, "echo")
	if err != nil || m.ProtocolVersion != 1 || m.Version != "" || m.CreatedAt != "" || m.Author != "" {
		t.Fatal("minimal registration rejected", m, err)
	}
	m.Version = "1.2.3-beta.1"
	m.CreatedAt = "2026-10-10T10:00:00+08:00"
	m.UpdatedAt = "2026-10-10T11:00:00+08:00"
	m.Author = "maintainer"
	m.License = "MIT"
	m.Tags = []string{"filesystem", "skill"}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseManifest(append([]byte{0xef, 0xbb, 0xbf}, data...), "echo")
	if err != nil || got.Version != m.Version || got.CreatedAt != m.CreatedAt || got.UpdatedAt != m.UpdatedAt || got.Author != m.Author || got.License != m.License || len(got.Tags) != 2 {
		t.Fatal("maintenance metadata not preserved", got, err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	fields["unexpected"] = true
	data, _ = json.Marshal(fields)
	if _, err := parseManifest(data, "echo"); !errors.Is(err, apperrors.ErrInvalidArgument) {
		t.Fatal("unknown registration field accepted", err)
	}
}
