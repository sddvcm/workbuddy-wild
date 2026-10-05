// login.go — WorkBuddy CN OAuth 登录（与 CPA 插件 /root/qoderwork/workbuddy/oauth.go
// 的 handleStartLogin + handlePollLogin 逐字一致的实现，CN realm only）。
//
// 三个子命令，由 login.sh 顺序驱动：
//
//	login url   → POST /v2/plugin/auth/state?platform=CLI 拿 state+authUrl，
//	              state 落 /tmp/wb2api-login-state.json，stdout 打印授权 URL
//	login poll  → 读 state，GET /v2/plugin/auth/token?state= 一次，
//	              成功再 GET /v2/plugin/login/account?state= 拿 uid/nickname，
//	              stdout 打印完整 token+account JSON
//	login auth  → 同 poll，但 stdout 只打印一行「账号文件绝对路径」，
//	              并直接以服务端 auth 嵌套形（auth{} + account{}）落盘到
//	              <auth_dir>/workbuddy-<uid>.json。
//	              ★ 这个子命令的存在就是为了消除容器里对 jq 的依赖。
//
// 无 PKCE（workbuddy 设备流由服务端签发 state，与 qoderwork 不同）。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 与 /root/qoderwork/workbuddy/main.go:82-96 完全一致的常量（CN only）
const (
	upstreamBaseCN  = "https://copilot.tencent.com"
	clientUA        = "CLI/2.63.2 CodeBuddy/2.63.2"
	originReferer   = "https://www.codebuddy.cn"
	endpointAuthState  = upstreamBaseCN + "/v2/plugin/auth/state?platform=CLI"
	endpointLoginAcct  = upstreamBaseCN + "/v2/plugin/login/account?state="
	endpointAuthToken  = upstreamBaseCN + "/v2/plugin/auth/token?state="
	stateFile          = "/tmp/wb2api-login-state.json"
)

// commonHeaders 与 main.go:496-503 一致
func commonHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", originReferer)
	req.Header.Set("Referer", originReferer+"/")
	req.Header.Set("User-Agent", clientUA)
}

// apiEnvelope 与 main.go:429-433 一致
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// doJSON 与 oauth.go:33-66 一致：{code,msg,data} 信封，code!=0 → error
func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	} else {
		commonHeaders(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "login: "+format+"\n", args...)
	os.Exit(1)
}

type loginState struct {
	State string `json:"state"`
}

// pollBundle 是 url 之后一次轮询拿到的全部凭证。
type pollBundle struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
	Domain       string
	UID          string
	EnterpriseID string
	Nickname     string
}

// doPoll 执行「读 state → 换 token → 查 account」全流程，供 poll/auth 共用。
// 返回的 error 已经是可以直接展示给用户的中文提示。
func doPoll(client *http.Client) (*pollBundle, error) {
	raw, err := os.ReadFile(stateFile)
	if err != nil {
		return nil, fmt.Errorf("read state: %v (先跑 login url)", err)
	}
	var ls loginState
	if err := json.Unmarshal(raw, &ls); err != nil {
		return nil, fmt.Errorf("parse state: %v", err)
	}
	// handlePollLogin (oauth.go:108-162)：auth/token 是权威登录状态端点，
	// pending 时业务 code 非 0（"login ing"），完成时 code=0 + token bundle
	tokRaw, status, errTok := doJSON(client, http.MethodGet, endpointAuthToken+ls.State, nil, nil)
	if errTok != nil {
		if status == 0 || status >= 500 {
			return nil, fmt.Errorf("token endpoint error: %v", errTok)
		}
		return nil, fmt.Errorf("登录未完成（waiting for login）。请确认已在浏览器完成登录再按回车")
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		return nil, fmt.Errorf("登录未完成（waiting for login）。请确认已在浏览器完成登录再按回车")
	}
	// login/account 拿 uid/nickname（带 Bearer）
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	acctHeaders := func(r *http.Request) {
		commonHeaders(r)
		r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	}
	if acctRaw, _, errAcct := doJSON(client, http.MethodGet, endpointLoginAcct+ls.State, acctHeaders, nil); errAcct == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	}
	b := &pollBundle{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresIn:    tok.ExpiresIn,
		Domain:       tok.Domain,
		UID:          acct.UID,
		EnterpriseID: acct.EnterpriseID,
		Nickname:     acct.Nickname,
	}
	os.Remove(stateFile)
	return b, nil
}

// authFile 是服务端 auth 文件格式（嵌套形）：auth{...} + account{...}。
// 与 internal/auth/auth.go 的解析结构逐字段对齐。
type authFile struct {
	Auth struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		Domain       string `json:"domain"`
		ApiHost      string `json:"apiHost"`
		MachineID    string `json:"machineId"`
		DeviceID     string `json:"deviceId"`
	} `json:"auth"`
	Account struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	} `json:"account"`
}

// writeAuthFile 把 poll 结果落盘为 <authDir>/workbuddy-<uid>.json（0600）。
// 返回写入的绝对路径。
func writeAuthFile(b *pollBundle, authDir string) (string, error) {
	if strings.TrimSpace(b.UID) == "" {
		return "", fmt.Errorf("未获取到 uid，登录可能未完成")
	}
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir %s: %v", authDir, err)
	}
	var af authFile
	af.Auth.AccessToken = b.AccessToken
	af.Auth.RefreshToken = b.RefreshToken
	af.Auth.ExpiresAt = time.Now().Unix() + b.ExpiresIn
	af.Auth.Domain = b.Domain
	af.Account.UID = b.UID
	af.Account.EnterpriseID = b.EnterpriseID
	af.Account.Nickname = b.Nickname
	data, err := json.MarshalIndent(af, "", "  ")
	if err != nil {
		return "", err
	}
	dest := filepath.Join(authDir, "workbuddy-"+b.UID+".json")
	if err := os.WriteFile(dest, data, 0o600); err != nil {
		return "", fmt.Errorf("write %s: %v", dest, err)
	}
	return dest, nil
}

func main() {
	if len(os.Args) < 2 {
		fatal("usage: login <url|poll|auth>")
	}
	// 每个流程独立 cookie jar（oauth.go:22-29：多账号登录互不串会话）
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: 30 * time.Second, Jar: jar}

	switch os.Args[1] {
	case "url":
		// handleStartLogin (oauth.go:68-87)
		data, _, err := doJSON(client, http.MethodPost, endpointAuthState, nil, bytes.NewReader([]byte("{}")))
		if err != nil {
			fatal("auth state failed: %v", err)
		}
		var st struct {
			State   string `json:"state"`
			AuthURL string `json:"authUrl"`
		}
		if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
			fatal("auth state: missing state or authUrl")
		}
		raw, _ := json.Marshal(loginState{State: st.State})
		if err := os.WriteFile(stateFile, raw, 0o600); err != nil {
			fatal("write state: %v", err)
		}
		fmt.Println(st.AuthURL)

	case "poll":
		b, err := doPoll(client)
		if err != nil {
			fatal("%v", err)
		}
		out := map[string]any{
			"access_token":  b.AccessToken,
			"refresh_token": b.RefreshToken,
			"expires_in":    b.ExpiresIn,
			"domain":        b.Domain,
			"uid":           b.UID,
			"enterprise_id": b.EnterpriseID,
			"nickname":      b.Nickname,
		}
		oraw, _ := json.Marshal(out)
		fmt.Println(string(oraw))

	case "auth":
		// 用法：login auth [authDir]   —— authDir 缺省 /data/auths
		authDir := "/data/auths"
		if len(os.Args) >= 3 && strings.TrimSpace(os.Args[2]) != "" {
			authDir = os.Args[2]
		}
		b, err := doPoll(client)
		if err != nil {
			fatal("%v", err)
		}
		dest, err := writeAuthFile(b, authDir)
		if err != nil {
			fatal("%v", err)
		}
		fmt.Println(dest)

	default:
		fatal("unknown subcommand %q (want url|poll|auth)", os.Args[1])
	}
}
