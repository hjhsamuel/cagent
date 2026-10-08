package observability

import (
	"context"
	"errors"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/sirupsen/logrus"
)

// safeFormatter 在副本上过滤结构化字段，避免更改调用者持有的 Entry。
// 日志消息必须是代码中的固定事件文本；此处不尝试猜测任意自然语言中的秘密。
// 非白名单字段一律移除，error 只保留稳定类别，绝不调用任意错误的 Error/String。
type safeFormatter struct{ next logrus.Formatter }

func (f safeFormatter) Format(e *logrus.Entry) ([]byte, error) {
	copy := *e
	copy.Data = logrus.Fields{}
	for k, v := range e.Data {
		switch k {
		case "request_id", "session_id", "run_id", "task_id", "invocation_id", "process_id", "method", "route", "status", "address":
			// 不格式化未知对象，防止 Stringer 借助受信字段输出凭据。
			switch x := v.(type) {
			case string:
				if len(x) <= 256 {
					copy.Data[k] = x
				}
			case int:
				copy.Data[k] = x
			}
		case "error":
			code := "internal_error"
			if err, ok := v.(error); ok {
				for _, kind := range []apperrors.Kind{apperrors.ErrOverloaded, apperrors.ErrInvalidArgument, apperrors.ErrNotFound, apperrors.ErrConflict, apperrors.ErrUnsupported} {
					if errors.Is(err, kind) {
						code = string(kind)
						break
					}
				}
				if errors.Is(err, context.Canceled) {
					code = "cancelled"
				}
				if errors.Is(err, context.DeadlineExceeded) {
					code = "timeout"
				}
			}
			copy.Data[k] = code
		}
	}
	return f.next.Format(&copy)
}
