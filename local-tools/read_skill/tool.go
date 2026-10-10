package main

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/hjhsamuel/cagent/pkg/localtool"
)

type Config struct {
	SkillsDir     string `json:"skills_dir"`
	MaxSkillBytes int    `json:"max_skill_bytes,omitempty"`
}

func Handle(req localtool.Request) (localtool.Response, error) {
	fail := func(message string) (localtool.Response, error) { return localtool.Response{Error: message}, nil }
	var cfg Config
	if localtool.Decode(req.Config, &cfg) != nil || strings.TrimSpace(cfg.SkillsDir) == "" || cfg.MaxSkillBytes < 0 || cfg.MaxSkillBytes > 4<<20 {
		return fail("invalid read_skill configuration")
	}
	if cfg.MaxSkillBytes == 0 {
		cfg.MaxSkillBytes = 256 << 10
	}
	var args struct {
		Name string `json:"name"`
	}
	if localtool.Decode(req.Arguments, &args) != nil || args.Name == "." || !fs.ValidPath(args.Name) || strings.ContainsAny(args.Name, `\:`) {
		return fail("invalid relative skill name")
	}
	root, err := os.OpenRoot(cfg.SkillsDir)
	if err != nil {
		return fail("configured skills directory is not accessible")
	}
	defer root.Close()
	f, err := root.Open(filepath.Join(filepath.FromSlash(args.Name), "SKILL.md"))
	if err != nil {
		return fail("skill not found or not accessible within configured directory")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fail("skill must be a regular SKILL.md file")
	}
	if info.Size() > int64(cfg.MaxSkillBytes) {
		return fail("skill exceeds configured read limit")
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(cfg.MaxSkillBytes)+1))
	if err != nil {
		return fail("cannot read skill file")
	}
	if len(data) > cfg.MaxSkillBytes {
		return fail("skill exceeds configured read limit")
	}
	if !utf8.Valid(data) {
		return fail("skill must contain UTF-8 text")
	}
	return localtool.Response{Text: string(data)}, nil
}
