package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/tool"
	"github.com/hjhsamuel/cagent/pkg/localtool"
)

// 被复制到工具目录的真实测试可执行文件，通过固定注册参数选择子进程入口。
func TestExecutableFixture(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-2] != "--" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	switch mode {
	case "hang":
		time.Sleep(10 * time.Second)
	case "stdout-limit":
		_, _ = io.WriteString(os.Stdout, strings.Repeat("x", 2<<20))
	case "stderr-limit":
		_, _ = io.WriteString(os.Stderr, strings.Repeat("x", 64<<10))
	case "bad-response":
		_, _ = io.WriteString(os.Stdout, "not JSON")
	case "two-responses":
		_, _ = io.WriteString(os.Stdout, `{"protocol_version":1,"text":"first"} {"protocol_version":1,"text":"second"}`)
	case "bad-version":
		_, _ = io.WriteString(os.Stdout, `{"protocol_version":2,"text":"wrong protocol"}`)
	case "unknown-response-field":
		_, _ = io.WriteString(os.Stdout, `{"protocol_version":1,"text":"value","call_id":"forged"}`)
	case "exit":
		_, _ = io.WriteString(os.Stderr, "sensitive provider diagnostic")
		os.Exit(2)
	default:
		err := localtool.Serve(os.Stdin, os.Stdout, func(req localtool.Request) (localtool.Response, error) {
			if mode == "business-error" {
				return localtool.Response{Text: "detail", Error: "order not found"}, nil
			}
			var args struct {
				Text string `json:"text"`
			}
			var config struct {
				Tag string `json:"tag"`
			}
			if localtool.Decode(req.Arguments, &args) != nil || localtool.Decode(req.Config, &config) != nil || os.Getenv("CAGENT_TEST_SECRET") != "" {
				return localtool.Response{}, errors.New("bad process request or inherited secret")
			}
			cwd, _ := os.Getwd()
			text := strings.Join([]string{req.Name, args.Text, config.Tag, req.Scope.TenantID, req.Scope.UserID, req.CallID, req.SessionID, req.RunID, req.Caller.AgentID, req.Caller.InvocationID, req.IdempotencyKey, filepath.Base(cwd)}, "|")
			return localtool.Response{Text: text}, nil
		})
		if err != nil {
			os.Exit(3)
		}
	}
	os.Exit(0)
}

func fixtureTool(t *testing.T, root, name, mode string) Manifest {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	filename := "fixture"
	if runtime.GOOS == "windows" {
		filename += ".exe"
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", filename), data, 0700); err != nil {
		t.Fatal(err)
	}
	m := Manifest{Name: name, Description: "真实进程测试", Version: "1.0.0", CreatedAt: "2026-10-10T10:00:00+08:00", Author: "test-maintainer", ProtocolVersion: 1, Executable: "bin/fixture", Args: []string{"-test.run=^TestExecutableFixture$", "--", mode}, InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`)}
	writeManifest(t, dir, m)
	return m
}

func writeManifest(t *testing.T, dir string, m Manifest) {
	t.Helper()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tool.json"), append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

func executableCall(name string) domain.ToolCall {
	return domain.ToolCall{Scope: domain.Scope{TenantID: "t", UserID: "u"}, SessionID: "s", RunID: "r", ID: "c", Caller: domain.AgentExecution{AgentID: "a", InvocationID: "i"}, Protocol: domain.ToolLocal, Name: name, Arguments: []byte(`{"text":"中文 $(no-shell) ; &"}`), IdempotencyKey: "key"}
}

func TestProcessDiscoveryRoutingConfigurationAndScope(t *testing.T) {
	t.Setenv("CAGENT_TEST_SECRET", "must-not-be-inherited")
	root := t.TempDir()
	for _, name := range []string{"first", "second"} {
		fixtureTool(t, root, name, "echo")
	}
	cfg := Config{ToolsDir: root, Config: map[string]json.RawMessage{"first": []byte(`{"tag":"one"}`), "second": []byte(`{"tag":"two"}`)}}
	definitions, err := Load(context.Background(), cfg, nil, Limits{Timeout: 5 * time.Second, MaxOutputBytes: 4096})
	if err != nil || len(definitions) != 2 {
		t.Fatal(definitions, err)
	}
	var entries []tool.Entry
	for _, d := range definitions {
		if d.Manifest.Version != "1.0.0" || d.Manifest.CreatedAt == "" || d.Manifest.Author != "test-maintainer" {
			t.Fatal("lost registration metadata", d.Manifest)
		}
		entries = append(entries, tool.Entry{Scope: executableCall(d.Descriptor.Name).Scope, ConnectionID: "local", Descriptor: d.Descriptor, Executor: d.Executor})
		// 对外描述和配置的后续修改不能改变执行器快照。
		d.Manifest.Args[2] = "exit"
	}
	cfg.Config["first"][8] = 'X'
	catalog, err := tool.NewCatalog(entries, tool.Limits{Timeout: 5 * time.Second, MaxInputBytes: 1024, MaxOutputBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"first", "second"} {
		call := executableCall(name)
		executor, err := catalog.Resolve(context.Background(), call.Scope, call.Protocol, call.Name)
		if err != nil {
			t.Fatal(err)
		}
		out, err := executor.Execute(context.Background(), call)
		tag := []string{"one", "two"}[i]
		want := name + "|中文 $(no-shell) ; &|" + tag + "|t|u|c|s|r|a|i|key|" + name
		if err != nil || out.Result == nil || out.Result.Error != "" || out.Result.Parts[0].Text != want || out.ValidateForCall(call) != nil {
			t.Fatal(out, err, want)
		}
		call.Scope.UserID = "other"
		if _, err := executor.Execute(context.Background(), call); !errors.Is(err, apperrors.ErrNotFound) {
			t.Fatal("captured executor bypassed scope", err)
		}
	}
}

func TestProcessFailuresAndCancellation(t *testing.T) {
	root := t.TempDir()
	m := fixtureTool(t, root, "fixture", "echo")
	for _, mode := range []string{"hang", "stdout-limit", "stderr-limit", "bad-response", "two-responses", "bad-version", "unknown-response-field", "exit", "business-error"} {
		t.Run(mode, func(t *testing.T) {
			m.Args[2] = mode
			writeManifest(t, filepath.Join(root, "fixture"), m)
			definitions, err := Load(context.Background(), Config{ToolsDir: root}, []string{"fixture"}, Limits{Timeout: 3 * time.Second, MaxOutputBytes: 1024})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if mode == "hang" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			started := time.Now()
			out, err := definitions[0].Executor.Execute(ctx, executableCall("fixture"))
			if mode == "business-error" {
				if err != nil || out.Result == nil || out.Result.Error != "order not found" || out.Result.Parts[0].Text != "detail" {
					t.Fatal(out, err)
				}
				return
			}
			if err == nil || strings.Contains(fmt.Sprint(err), "sensitive") || out.Result != nil {
				t.Fatal("invalid subprocess result accepted or diagnostic leaked", out, err)
			}
			if mode == "hang" && (!errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 2*time.Second) {
				t.Fatal("timeout failed to stop process promptly", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Load(ctx, Config{ToolsDir: root}, nil, Limits{Timeout: time.Second, MaxOutputBytes: 1024}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
