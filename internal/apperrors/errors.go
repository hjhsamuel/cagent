// Package apperrors 定义各层共享的错误约定，仅依赖标准库。
// 领域、应用及适配器均可引用本包，不需要反向依赖存储或传输层。
// HTTP 状态码、SDK 错误转换和重试策略由各自边界决定。
package apperrors

import "fmt"

// Kind 是稳定的机器可读错误类别，同时实现 error，可直接用于 errors.Is。
// 类别使用常量避免被重新赋值；调用方应比较类别，不应解析 Error() 文本。
type Kind string

const (
	// ErrOverloaded 表示本实例容量已满；请求可退避重试，不自动重放外部副作用。
	ErrOverloaded Kind = "overloaded"
	// ErrInvalidArgument 表示缺失字段、非法取值或资源关联不一致。
	// 它不表示认证失败，也不能代替存储层的作用域过滤。
	ErrInvalidArgument Kind = "invalid_argument"
	// ErrNotFound 表示在已授权作用域内未找到资源，不透露其他作用域的存在性。
	ErrNotFound Kind = "not_found"
	// ErrConflict 表示资源版本等并发约束冲突；是否重试由具体操作决定。
	ErrConflict Kind = "conflict"
	// ErrUnsupported 表示合法请求所需的能力未被实现，例如提供方不支持取消。
	// 未知协议等非法输入应归为 ErrInvalidArgument，而不是能力缺失。
	ErrUnsupported Kind = "unsupported"
)

// Error 返回稳定类别标识，不携带资源 ID 或外部系统细节。
func (k Kind) Error() string { return string(k) }

// Error 保存可向调用方展示的细节与仅供内部诊断的原因链。
// 字段私有且构造后不修改，避免调用方修改分类或将 cause 自动序列化到响应中。
// 使用 errors.As(err, &detail)（detail 为 *Error）提取最外层公共错误。
// DTO 应显式选取 Kind、Field、Message；不能遍历原因链并将其返回给客户端。
type Error struct {
	kind    Kind
	field   string
	message string
	cause   error
}

// New 创建没有底层原因的公共错误。kind 应使用本包定义的类别。
// field 是代码定义的字段路径（如 scope.tenant_id），允许为空；message 是安全说明。
// 两者必须来自可信静态文本，不得拼入凭据、提示词、参数值或连接信息。
// 本包不会猜测或清洗任意文本中的秘密，安全说明由创建错误的边界负责选择。
func New(kind Kind, field, message string) *Error {
	return &Error{kind: kind, field: field, message: message}
}

// Wrap 为已有失败附加类别和安全说明；cause 为 nil 时返回真正的 nil，便于直接返回。
// 原因只保存在 Unwrap 链中，Error 和 fmt 格式化不会拼接 cause.Error()。
// 取消和超时不新增公共类别：包装 context.Canceled / DeadlineExceeded 后，
// errors.Is 仍可识别标准错误，调用方应优先处理这些生命周期信号。
// kind、field、message 的来源约束与 New 相同；本函数不会推断底层错误类别。
func Wrap(kind Kind, field, message string, cause error) error {
	if cause == nil {
		return nil
	}
	return &Error{kind: kind, field: field, message: message, cause: cause}
}

// Kind 返回当前层的类别。多层包装可能保留不同类别，errors.Is 会遍历整条链；
// 需要当前边界的分类时，应通过 errors.As 提取最外层 Error 再读取 Kind。
func (e *Error) Kind() Kind { return e.kind }

// Field 返回该错误所指向的字段路径；无具体字段的操作错误返回空字符串。
// 外层 fmt.Errorf 的上下文不会改写这里的路径。
func (e *Error) Field() string { return e.field }

// Message 返回创建者提供的安全说明，不含底层原因。
func (e *Error) Message() string { return e.message }

// Error 仅组合类别、字段路径和安全说明。文本供人阅读，不作为稳定协议。
func (e *Error) Error() string {
	text := e.kind.Error()
	if e.field != "" {
		text += ": " + e.field
	}
	if e.message != "" {
		text += ": " + e.message
	}
	return text
}

// Format 让包括 %+v、%#v 在内的 fmt 输出都只格式化安全文本，
// 防止调试格式展开私有 cause。显式 Unwrap 后的内容仍属于敏感诊断数据。
func (e *Error) Format(state fmt.State, verb rune) {
	fmt.Fprintf(state, fmt.FormatString(state, verb), e.Error())
}

// Is 只比较当前层类别，不比较错误说明或字段，也不手工遍历原因链。
// 原因链遍历由标准 errors.Is 完成，因此底层哨兵与 context 错误不会丢失。
func (e *Error) Is(target error) bool {
	kind, ok := target.(Kind)
	return ok && e.kind == kind
}

// Unwrap 保留底层原始错误，支持 errors.Is、errors.As 和内部诊断。
// 这里返回的错误可能含敏感数据，不得直接写入对外响应或未经脱敏的日志。
func (e *Error) Unwrap() error { return e.cause }
