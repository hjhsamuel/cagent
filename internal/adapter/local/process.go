package local

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/tool"
	"github.com/hjhsamuel/cagent/pkg/localtool"
)

type processExecutor struct {
	manifest   Manifest
	directory  string
	executable string
	config     json.RawMessage
	limits     Limits
}

func (e *processExecutor) Execute(ctx context.Context, call domain.ToolCall) (domain.ToolOutcome, error) {
	if err := ctx.Err(); err != nil {
		return domain.ToolOutcome{}, err
	}
	if err := call.Validate(); err != nil {
		return domain.ToolOutcome{}, err
	}
	if call.Protocol != domain.ToolLocal || call.Name != e.manifest.Name {
		return domain.ToolOutcome{}, apperrors.ErrNotFound
	}
	// 管理员替换部署文件后也重新检查可执行文件路径边界。
	path, err := resolveExecutable(e.directory, e.manifest.Executable)
	if err != nil || path != e.executable {
		return domain.ToolOutcome{}, apperrors.New(apperrors.ErrInvalidArgument, "local.executable", "executable path changed or is unavailable")
	}
	request := localtool.Request{
		ProtocolVersion: localtool.ProtocolVersion, Name: call.Name, CallID: call.ID,
		Scope:     localtool.Scope{TenantID: call.Scope.TenantID, UserID: call.Scope.UserID},
		SessionID: call.SessionID, RunID: call.RunID,
		Caller:         localtool.Caller{AgentID: call.Caller.AgentID, InvocationID: call.Caller.InvocationID, ParentInvocationID: call.Caller.ParentInvocationID},
		IdempotencyKey: call.IdempotencyKey, Arguments: call.Arguments, Config: e.config,
	}
	input, err := json.Marshal(request)
	if err != nil {
		return domain.ToolOutcome{}, apperrors.ErrInvalidArgument
	}
	processCtx, cancel := context.WithTimeout(ctx, e.limits.Timeout)
	defer cancel()
	stdout := &limitedBuffer{limit: e.limits.MaxOutputBytes, cancel: cancel}
	stderr := &limitedBuffer{limit: 32 << 10, cancel: cancel}
	cmd := exec.CommandContext(processCtx, e.executable, e.manifest.Args...)
	cmd.Dir = e.directory
	cmd.Env = processEnvironment()
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// 派生进程保留管道时也有界等待，避免工具退出后永久阻塞服务。
	cmd.WaitDelay = time.Second
	configureProcess(cmd)
	err = cmd.Run()
	if stdout.exceeded || stderr.exceeded {
		return domain.ToolOutcome{}, apperrors.New(apperrors.ErrInvalidArgument, "local.output", "local process output exceeded limit")
	}
	if processCtx.Err() != nil {
		return domain.ToolOutcome{}, processCtx.Err()
	}
	if err != nil {
		// 不向模型或日志回显 stderr、可执行文件绝对路径或请求内容。
		return domain.ToolOutcome{}, tool.SafeError(err)
	}
	var response localtool.Response
	if !utf8.Valid(stdout.Bytes()) || localtool.Decode(stdout.Bytes(), &response) != nil || response.ProtocolVersion != localtool.ProtocolVersion {
		return domain.ToolOutcome{}, apperrors.New(apperrors.ErrInvalidArgument, "local.response", "invalid local process response")
	}
	return domain.ToolOutcome{Result: &domain.ToolResult{CallID: call.ID, Parts: []domain.Part{{Kind: domain.PartText, Text: response.Text}}, Error: response.Error}}, nil
}

var errOutputLimit = errors.New("local process output limit exceeded")

type limitedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	cancel   context.CancelFunc
	exceeded bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.buffer.Len() {
		b.exceeded = true
		b.cancel()
		return 0, errOutputLimit
	}
	return b.buffer.Write(p)
}

func (b *limitedBuffer) Bytes() []byte { return b.buffer.Bytes() }

// 不继承服务的模型密钥、数据库凭据或其他连接的凭据。业务配置由 stdin 显式传入。
func processEnvironment() []string {
	allowed := map[string]bool{"PATH": true, "SYSTEMROOT": true, "WINDIR": true, "TEMP": true, "TMP": true, "TMPDIR": true, "LANG": true, "LC_ALL": true}
	var env []string
	for _, item := range os.Environ() {
		key, _, ok := strings.Cut(item, "=")
		if ok && allowed[strings.ToUpper(key)] {
			env = append(env, item)
		}
	}
	return env
}
