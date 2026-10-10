package local

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/tool"
)

type Config struct {
	ToolsDir string                     `json:"tools_dir"`
	Config   map[string]json.RawMessage `json:"config,omitempty"`
}

type Limits struct {
	Timeout        time.Duration
	MaxOutputBytes int
}

type Definition struct {
	Manifest   Manifest
	Descriptor tool.Descriptor
	Executor   tool.Executor
}

// Load 仅扫描 tools_dir 的一级工具目录。白名单非空时只读取选中工具的 tool.json。
// 发现期间不会运行可执行文件；新工具通过部署目录加入，无需编译服务。
func Load(ctx context.Context, cfg Config, names []string, limits Limits) ([]Definition, error) {
	if strings.TrimSpace(cfg.ToolsDir) == "" || limits.Timeout <= 0 || limits.MaxOutputBytes <= 0 || limits.MaxOutputBytes > 8<<20 {
		return nil, apperrors.New(apperrors.ErrInvalidArgument, "local", "tools directory and valid process limits are required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(cfg.ToolsDir)
	if err != nil {
		return nil, tool.SafeError(err)
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrInvalidArgument, "local.tools_dir", "cannot open tools directory", err)
	}
	defer root.Close()
	selected := append([]string(nil), names...)
	if len(selected) == 0 {
		entries, err := fs.ReadDir(root.FS(), ".")
		if err != nil {
			return nil, tool.SafeError(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				selected = append(selected, entry.Name())
			}
		}
	}
	sort.Strings(selected)
	var definitions []Definition
	for i, name := range selected {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !toolName.MatchString(name) || (i > 0 && selected[i-1] == name) {
			return nil, apperrors.New(apperrors.ErrInvalidArgument, "local.tools", "invalid or duplicate tool directory name")
		}
		info, err := root.Lstat(name)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, apperrors.New(apperrors.ErrInvalidArgument, "local.tools", "tool directory missing or not a regular directory")
		}
		dir, err := root.OpenRoot(name)
		if err != nil {
			return nil, tool.SafeError(err)
		}
		m, err := readManifest(dir, name)
		_ = dir.Close()
		if err != nil {
			return nil, err
		}
		toolDir := filepath.Join(abs, name)
		executable, err := resolveExecutable(toolDir, m.Executable)
		if err != nil {
			return nil, err
		}
		settings := json.RawMessage(`{}`)
		if m.Config != nil {
			settings = append(json.RawMessage(nil), m.Config...)
		}
		if configured, ok := cfg.Config[name]; ok {
			var object map[string]json.RawMessage
			if json.Unmarshal(configured, &object) != nil || object == nil {
				return nil, apperrors.New(apperrors.ErrInvalidArgument, "local.config", "tool config must be a JSON object")
			}
			settings = append(json.RawMessage(nil), configured...)
		}
		execManifest := m
		execManifest.Args = append([]string(nil), m.Args...)
		definitions = append(definitions, Definition{
			Manifest:   m,
			Descriptor: tool.Descriptor{Name: m.Name, Protocol: domain.ToolLocal, Description: m.Description, InputSchema: append([]byte(nil), m.InputSchema...)},
			Executor:   &processExecutor{manifest: execManifest, directory: toolDir, executable: executable, config: settings, limits: limits},
		})
	}
	return definitions, nil
}

func readManifest(root *os.Root, name string) (Manifest, error) {
	f, err := root.Open("tool.json")
	if err != nil {
		return Manifest{}, apperrors.Wrap(apperrors.ErrInvalidArgument, "local.tool.json", "cannot read tool registration", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return Manifest{}, apperrors.New(apperrors.ErrInvalidArgument, "local.tool.json", "tool registration must be a regular file")
	}
	const maxManifestBytes = 256 << 10
	data, err := io.ReadAll(io.LimitReader(f, maxManifestBytes+1))
	if err != nil || len(data) > maxManifestBytes {
		return Manifest{}, apperrors.New(apperrors.ErrInvalidArgument, "local.tool.json", "tool registration cannot be read within size limit")
	}
	return parseManifest(data, name)
}

func resolveExecutable(dir, name string) (string, error) {
	bad := func() (string, error) {
		return "", apperrors.New(apperrors.ErrInvalidArgument, "local.executable", "executable must be an accessible file within its tool directory")
	}
	if name == "." || !fs.ValidPath(name) || strings.ContainsAny(name, `\:`) {
		return bad()
	}
	dirInfo, err := os.Lstat(dir)
	if err != nil || !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 {
		return bad()
	}
	if runtime.GOOS == "windows" && filepath.Ext(name) == "" {
		name += ".exe"
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return bad()
	}
	defer root.Close()
	if runtime.GOOS == "windows" && !strings.EqualFold(filepath.Ext(name), ".exe") {
		return bad()
	}
	f, err := root.Open(filepath.FromSlash(name))
	if err != nil {
		return bad()
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0111 == 0) {
		return bad()
	}
	return filepath.Join(dir, filepath.FromSlash(name)), nil
}
