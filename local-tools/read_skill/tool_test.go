package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hjhsamuel/cagent/pkg/localtool"
)

func TestSkillContentsPathsLimitsAndEncoding(t *testing.T) {
	root := t.TempDir()
	content := "---\nname: review\n---\n# 中文审查\n"
	for name, data := range map[string][]byte{"team/review": []byte(content), "binary": {0xff, 0xfe}, "large": []byte(strings.Repeat("x", 1025))} {
		dir := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	settings, _ := json.Marshal(Config{SkillsDir: root, MaxSkillBytes: 1024})
	for _, name := range []string{"team/review", "", ".", "..", "../private", "team/../../private", "/private", `C:\private`, `team\..\private`, "team:stream", "missing", "binary", "large"} {
		args, _ := json.Marshal(map[string]string{"name": name})
		out, err := Handle(localtool.Request{Arguments: args, Config: settings})
		if err != nil {
			t.Fatal(err)
		}
		if name == "team/review" {
			if out.Text != content || out.Error != "" {
				t.Fatal("skill was not read in full", out)
			}
		} else if out.Error == "" || out.Text != "" {
			t.Fatal("invalid skill accepted", name, out)
		}
	}
	for _, cfg := range []string{`{}`, `{"skills_dir":"x","max_skill_bytes":-1}`, `{"skills_dir":"x","max_skill_bytes":4194305}`, `{"skills_dir":"x","unknown":true}`} {
		out, err := Handle(localtool.Request{Config: []byte(cfg), Arguments: []byte(`{"name":"review"}`)})
		if err != nil || out.Error == "" {
			t.Fatal("bad tool config accepted", out, err)
		}
	}
}
