// Package login_trae 实现 TraeWork OAuth 回调登录。
package login_trae

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/auth"
	"github.com/rockswang/workbuddy-wild/internal/traework"
)

var ErrPending = errors.New("login pending")

type Result struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
	Domain       string
	ApiHost      string
	MachineID    string
	DeviceID     string
	UID          string
	EnterpriseID string
	Nickname     string
}

type state struct {
	MachineID    string `json:"machineId"`
	DeviceID     string `json:"deviceId"`
	RefreshToken string `json:"refreshToken,omitempty"`
	Host         string `json:"host,omitempty"`
	Err          string `json:"err,omitempty"`
}

func NewClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

// Start 启动本地一次性回调监听，返回 Trae 授权 URL。
//
// deviceID 参数：调用方应传入**客户端真实注册设备号**（见 ReadClientDeviceID /
// NextClientDeviceID）。传空则本函数自行读取；仍读不到时回退随机值。
//
// 为什么必须用真实设备号：服务端按「注册指纹」校验 device id，签到 claim 接口
// 对未注册设备返回 9074。2026-09-30 单变量实测（同账号同 token）：
//
//	随机 32 位 hex + {"req_source":1} → 9074
//	真实注册号       + {"req_source":1} → 成功
//
// 详见 device.go。
func Start(client *http.Client, statePath, deviceID string) (string, error) {
	machineID := randHex(16)
	if strings.TrimSpace(deviceID) == "" {
		deviceID = ReadClientDeviceID()
	}
	if deviceID == "" {
		// 没有客户端设备号时回退随机值。此时签到很可能失败（9074），
		// 但其它功能（积分查询、API 代理）仍可正常使用。
		deviceID = randHex(16)
		log.Printf("traework device: 未取到客户端设备号，回退随机值；签到可能因设备校验失败（9074）")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := ln.Addr().String()
	callback := "http://" + addr + "/authorize"
	st := state{MachineID: machineID, DeviceID: deviceID}
	if err := writeState(statePath, st); err != nil {
		_ = ln.Close()
		return "", err
	}

	srv := &http.Server{ReadHeaderTimeout: 15 * time.Second}
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		st.RefreshToken = q.Get("refreshToken")
		st.Host = q.Get("host")
		if st.Host == "" {
			st.Host = traework.OAuthHost
		}
		if st.RefreshToken == "" {
			st.Err = "missing refreshToken in callback"
		}
		_ = writeState(statePath, st)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body style='font-family:sans-serif;padding:24px'>TraeWork 登录已完成，可以关闭此页面。</body></html>"))
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		}()
	})
	go func() { _ = srv.Serve(ln) }()
	go func() {
		time.Sleep(5 * time.Minute)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	u, _ := url.Parse(traework.ConsoleHost + "/authorization")
	v := u.Query()
	v.Set("login_version", "1")
	v.Set("auth_from", "solo")
	v.Set("login_channel", "native_ide")
	v.Set("plugin_version", "2.3.62834")
	v.Set("auth_type", "local")
	v.Set("client_id", traework.ClientID)
	v.Set("redirect", "0")
	v.Set("login_trace_id", randHex(8))
	v.Set("auth_callback_url", callback)
	v.Set("machine_id", machineID)
	v.Set("device_id", deviceID)
	v.Set("x_device_id", deviceID)
	v.Set("x_machine_id", machineID)
	v.Set("x_device_brand", "PC")
	v.Set("x_device_type", "PC")
	v.Set("x_os_version", "1.0")
	v.Set("x_app_version", traework.IdeVersion)
	v.Set("x_app_type", "stable")
	u.RawQuery = v.Encode()
	_ = client
	return u.String(), nil
}

// Poll 检查回调是否已写入 state，完成后 ExchangeToken + GetUserInfo。
func Poll(client *http.Client, statePath string) (Result, error) {
	var st state
	raw, err := os.ReadFile(statePath)
	if err != nil {
		return Result{}, err
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return Result{}, err
	}
	if st.Err != "" {
		return Result{}, errors.New(st.Err)
	}
	if st.RefreshToken == "" {
		return Result{}, ErrPending
	}
	if st.Host == "" {
		st.Host = traework.OAuthHost
	}
	c := traework.New()
	c.HTTP = client
	a := &auth.Auth{Kind: "traework", RefreshToken: st.RefreshToken, ApiHost: st.Host, MachineID: st.MachineID, DeviceID: st.DeviceID, Domain: "trae.cn"}
	if err := c.RefreshToken(a); err != nil {
		return Result{}, err
	}
	uid, nick, ent, err := c.GetUserInfo(a)
	if err != nil {
		return Result{}, err
	}
	_ = os.Remove(statePath)
	return Result{AccessToken: a.AccessToken, RefreshToken: a.RefreshToken, ExpiresAt: a.ExpiresAt, Domain: "trae.cn", ApiHost: st.Host, MachineID: st.MachineID, DeviceID: st.DeviceID, UID: uid, EnterpriseID: ent, Nickname: nick}, nil
}

// ── 无头（Docker/NAS）登录：两段式手工回调 ──────────────────────────────
//
// 桌面端能起 127.0.0.1 回调服务器是因为浏览器与程序在同机。容器里用户
// 的浏览器访问不到容器的 127.0.0.1，所以改成：
//   ① StartLocal 只生成授权 URL，不监听端口
//   ② 用户从浏览器地址栏复制回调 URL，交给 PollCallback 解析
//
// auth_callback_url 仍填 http://127.0.0.1:<端口>/authorize —— 作用是让
// 浏览器跳过去（用户看到"打不开"是预期的），从地址栏就能拿到带
// refreshToken 的完整 query。

// StartLocal 生成 TraeWork 授权 URL（不监听本地端口），并把机器号/设备号
// 写入 statePath 供 PollCallback 使用。
//
// deviceID / machineID 为空时随机生成；deviceID 强烈建议传入客户端真实值，
// 否则签到会因设备校验失败返回 9074（见 device.go 的实测记录）。
func StartLocal(client *http.Client, statePath, deviceID, machineID string) (string, error) {
	deviceID = strings.TrimSpace(deviceID)
	machineID = strings.TrimSpace(machineID)
	if deviceID == "" {
		deviceID = randNum16()
		log.Printf("traework login: 未提供设备号，已随机生成（签到可能返回 9074）")
	}
	if machineID == "" {
		machineID = randHex(16)
	}
	const cbPort = "18080"
	callback := "http://127.0.0.1:" + cbPort + "/authorize"

	st := state{MachineID: machineID, DeviceID: deviceID}
	if err := writeState(statePath, st); err != nil {
		return "", err
	}
	_ = client
	return buildAuthURL(callback, machineID, deviceID), nil
}

// PollCallback 用用户手工粘贴的回调 URL 完成登录：解析 refreshToken / host，
// 兑换 access token，拉取账号信息。
//
// 回调 URL 形如：
//
//	http://127.0.0.1:18080/authorize?isRedirect=true&scope=solo&data=...&refreshToken=<jwt>&loginTraceID=...&host=https://api.trae.com.cn&...
//
// 用户可能粘贴带 fragment 或被浏览器编码过的串，这里做宽松解析。
func PollCallback(client *http.Client, statePath, callbackURL string) (Result, error) {
	var st state
	if raw, err := os.ReadFile(statePath); err == nil {
		_ = json.Unmarshal(raw, &st)
	} else {
		return Result{}, fmt.Errorf("读取 state 失败（请先执行 url 子命令）: %w", err)
	}

	raw := strings.TrimSpace(callbackURL)
	// 用户偶尔会连引号一起复制，或漏掉协议头。
	raw = strings.Trim(raw, `'"`)
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Result{}, fmt.Errorf("回调 URL 解析失败: %w", err)
	}
	q := u.Query()
	// refreshToken 可能在 query，也可能（被某些浏览器）塞进 fragment。
	if q.Get("refreshToken") == "" && u.Fragment != "" {
		if fragQ, err := url.ParseQuery(strings.TrimPrefix(u.Fragment, "?")); err == nil {
			for k, v := range fragQ {
				if len(v) > 0 && q.Get(k) == "" {
					q.Set(k, v[0])
				}
			}
		}
	}
	rt := q.Get("refreshToken")
	if rt == "" {
		return Result{}, fmt.Errorf("回调 URL 里没有 refreshToken —— " +
			"请确认复制的是「登录完成后浏览器地址栏里的完整地址」（应以 ? 开头带一堆参数）")
	}
	host := q.Get("host")
	if host == "" {
		host = traework.OAuthHost
	}
	if strings.TrimSpace(st.DeviceID) == "" {
		return Result{}, fmt.Errorf("state 中缺少设备号，请重新执行 url 子命令")
	}

	c := traework.New()
	c.HTTP = client
	a := &auth.Auth{
		Kind: "traework", RefreshToken: rt, ApiHost: host,
		MachineID: st.MachineID, DeviceID: st.DeviceID, Domain: "trae.cn",
	}
	if err := c.RefreshToken(a); err != nil {
		return Result{}, fmt.Errorf("兑换 token 失败: %w", err)
	}
	uid, nick, ent, err := c.GetUserInfo(a)
	if err != nil {
		return Result{}, fmt.Errorf("获取账号信息失败: %w", err)
	}
	_ = os.Remove(statePath)
	return Result{
		AccessToken: a.AccessToken, RefreshToken: a.RefreshToken, ExpiresAt: a.ExpiresAt,
		Domain: "trae.cn", ApiHost: host,
		MachineID: st.MachineID, DeviceID: st.DeviceID,
		UID: uid, EnterpriseID: ent, Nickname: nick,
	}, nil
}

// buildAuthURL 组装授权 URL（桌面端与无头端共用同一套 query 参数）。
func buildAuthURL(callback, machineID, deviceID string) string {
	u, _ := url.Parse(traework.ConsoleHost + "/authorization")
	v := u.Query()
	v.Set("login_version", "1")
	v.Set("auth_from", "solo")
	v.Set("login_channel", "native_ide")
	v.Set("plugin_version", "2.3.62834")
	v.Set("auth_type", "local")
	v.Set("client_id", traework.ClientID)
	v.Set("redirect", "0")
	v.Set("login_trace_id", randHex(8))
	v.Set("auth_callback_url", callback)
	v.Set("machine_id", machineID)
	v.Set("device_id", deviceID)
	v.Set("x_device_id", deviceID)
	v.Set("x_machine_id", machineID)
	v.Set("x_device_brand", "PC")
	v.Set("x_device_type", "PC")
	v.Set("x_os_version", "1.0")
	v.Set("x_app_version", traework.IdeVersion)
	v.Set("x_app_type", "stable")
	u.RawQuery = v.Encode()
	return u.String()
}

// randNum16 生成 16 位十进制随机设备号（回退值，非真实注册设备）。
func randNum16() string {
	var b [16]byte
	for i := range b {
		b[i] = '0' + byte(randInt(10))
	}
	return string(b[:])
}

// randInt 返回 [0,n) 的随机整数（n<=0 时返回 0）。
func randInt(n int) int {
	if n <= 0 {
		return 0
	}
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0
	}
	v := int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3])
	if v < 0 {
		v = -v
	}
	return v % n
}

func SaveAuth(authDir string, r Result) (string, error) {
	if r.UID == "" {
		return "", fmt.Errorf("missing uid in result")
	}
	doc := map[string]any{
		"auth":    map[string]any{"accessToken": auth.EncryptSecret(r.AccessToken), "refreshToken": auth.EncryptSecret(r.RefreshToken), "expiresAt": r.ExpiresAt, "domain": r.Domain, "apiHost": r.ApiHost, "machineId": r.MachineID, "deviceId": r.DeviceID},
		"account": map[string]any{"uid": r.UID, "enterpriseId": r.EnterpriseID, "nickname": r.Nickname},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	_ = os.MkdirAll(authDir, 0o755)
	fp := filepath.Join(authDir, "trae-"+r.UID+".json")
	tmp := fp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, fp); err != nil {
		return "", err
	}
	return fp, nil
}

func writeState(path string, st state) error {
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	raw, _ := json.MarshalIndent(st, "", "  ")
	return os.WriteFile(path, raw, 0o600)
}

func randHex(n int) string { b := make([]byte, n); _, _ = rand.Read(b); return hex.EncodeToString(b) }
