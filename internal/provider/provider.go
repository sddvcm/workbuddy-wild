// Package provider 定义不同上游（workbuddy / traework）共用的最小接口。
// 只抽取 server/scheduler 必需能力，避免为未来平台过度设计。
package provider

import (
	"fmt"
	"io"
	"net/http"

	"github.com/rockswang/workbuddy-wild/internal/auth"
)

// Kind 平台标识，同时也是模型名前缀。
type Kind string

const (
	WorkBuddy Kind = "workbuddy"
	TraeWork  Kind = "traework"
)

func (k Kind) String() string { return string(k) }

// ErrKind 错误分类，驱动 pool 冷却状态机。
type ErrKind int

const (
	ErrNone        ErrKind = iota // 成功
	ErrHardCredit                 // 余额/权益不足 → 长冷却
	ErrSoftRate                   // 429 软限流 → 短冷却
	ErrSessionDead                // 登录态失效 → 禁用
	ErrNotFound                   // 404 上游偶发 → 短冷却不累计 errCount
	ErrBadModel                   // 请求方参数错误（模型 ID 不存在 / 参数非法）→ 与账号无关，不冷却、不轮换
	ErrServer                     // 5xx 上游故障
	ErrClient                     // 其他 4xx / 业务错误
	ErrUnknown                    // 已确认是错误但类别待定 → 交由调用方按 body 细分
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrBadModel:
		return "bad_model"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
	case ErrUnknown:
		return "unknown"
	default:
		return "none"
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// ModelInfo 动态/静态模型信息。
type ModelInfo struct {
	ID            string
	Name          string
	ContextWindow int64
	MaxTokens     int64
}

// Upstream 是 server/scheduler 依赖的最小上游能力集合。
type Upstream interface {
	RefreshToken(a *auth.Auth) error
	ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error)
	FetchModels(a *auth.Auth) ([]ModelInfo, error)
	UserResource(a *auth.Auth) (int64, error)
	DailyCheckin(a *auth.Auth) error
	Classify(status int, body string) ErrKind
	Stream(w http.ResponseWriter, r io.Reader) error
	Aggregate(r io.Reader) (map[string]any, error)
}

// StreamErrorReporter 是 Upstream 的**可选**扩展：流式转换过程中
// 遭遇上游错误时回调出来（错误本身仍以下发 SSE 事件的方式交给客户端）。
//
// 为什么需要它：错误可能出现在流的**中途**，超出 ChatStream 的预读嗅探
// 窗口。若不回调，错误只下发给客户端，服务端却已把它记为"账号成功"，
// 于是该冷却的账号没有被冷却（权益不足会被反复重试）。
//
// 实现方（traework）满足此接口；workbuddy 上游是 OpenAI 原始 SSE，
// 不做流内错误包装，因此不实现 —— handler 用类型断言按需取用。
type StreamErrorReporter interface {
	StreamWithError(w http.ResponseWriter, r io.Reader, onErr func(kind ErrKind, msg string)) error
}
