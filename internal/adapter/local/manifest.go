// Package local 从可信工具目录发现可执行文件，通过进程协议适配统一工具边界。
package local

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/pkg/localtool"
)

// Manifest 是 tool.json 中的注册配置。维护信息可选，不进入模型参数或进程请求。
type Manifest struct {
	Name            string          `json:"name"`
	Description     string          `json:"description"`
	Version         string          `json:"version,omitempty"`
	CreatedAt       string          `json:"created_at,omitempty"`
	UpdatedAt       string          `json:"updated_at,omitempty"`
	Author          string          `json:"author,omitempty"`
	License         string          `json:"license,omitempty"`
	Tags            []string        `json:"tags,omitempty"`
	ProtocolVersion int             `json:"protocol_version,omitempty"`
	Executable      string          `json:"executable"`
	Args            []string        `json:"args,omitempty"`
	InputSchema     json.RawMessage `json:"input_schema"`
	Config          json.RawMessage `json:"config,omitempty"`
}

var toolName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
var toolVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

func parseManifest(data []byte, directory string) (Manifest, error) {
	bad := func() (Manifest, error) {
		return Manifest{}, apperrors.New(apperrors.ErrInvalidArgument, "local.tool.json", "invalid tool registration or metadata")
	}
	if !utf8.Valid(data) {
		return bad()
	}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	var m Manifest
	if localtool.Decode(data, &m) != nil || !toolName.MatchString(m.Name) || m.Name != directory || strings.TrimSpace(m.Description) == "" {
		return bad()
	}
	if m.ProtocolVersion == 0 {
		m.ProtocolVersion = localtool.ProtocolVersion
	}
	if m.ProtocolVersion != localtool.ProtocolVersion || (m.Version != "" && !toolVersion.MatchString(m.Version)) {
		return bad()
	}
	if m.Config != nil {
		var object map[string]json.RawMessage
		if json.Unmarshal(m.Config, &object) != nil || object == nil {
			return bad()
		}
	}
	for _, timestamp := range []string{m.CreatedAt, m.UpdatedAt} {
		if timestamp != "" {
			if _, err := time.Parse(time.RFC3339, timestamp); err != nil {
				return bad()
			}
		}
	}
	var schema jsonschema.Schema
	if json.Unmarshal(m.InputSchema, &schema) != nil || schema.Type != "object" {
		return bad()
	}
	if _, err := schema.Resolve(nil); err != nil {
		return bad()
	}
	return m, nil
}
