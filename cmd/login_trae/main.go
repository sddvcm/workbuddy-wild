// login_trae 无头 TraeWork 登录助手（供 Docker/NAS 使用）。
//
// 与桌面端（internal/login_trae + app.go）的差异：
//   桌面端  用 net.Listen("127.0.0.1:0") 起本地回调服务器，浏览器直接把 refreshToken
//           回调进来，全程自动。
//   无头端  容器里没有浏览器、也起不了「用户能访问的 127.0.0.1 回调」，
//           因此改成**两段式手工流程**：
//             1) login_trae url       → 打印授权 URL（用户在自己电脑的浏览器打开）
//             2) 浏览器跳转失败后，从地址栏复制完整回调 URL
//             3) login_trae poll <回调URL> → 解析 refreshToken，换 token，落 auth 文件
//
// 设备号（★ 签到关键）：
//   Trae 服务端按「注册指纹」校验 device_id。随机值会让签到 claim 恒返 9074。
//   容器里读不到 Windows 客户端的 storage.json，所以必须由用户从客户端机器上
//   取值后用 --device-id 传进来（见 --help 的取值步骤）。
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/login_trae"
)

func main() {
	authDir := flag.String("auth-dir", envOr("WB2A_AUTH_DIR", "/data/auths"), "账号文件写入目录")
	deviceID := flag.String("device-id", "", "Trae 客户端真实注册设备号（16 位数字，签到必需——见下方说明）")
	machineID := flag.String("machine-id", "", "机器号（32 位 hex，可选；留空随机生成）")
	timeout := flag.Duration("timeout", 30*time.Second, "HTTP 超时")
	flag.Usage = usage
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	client := &http.Client{Timeout: *timeout}
	statePath := filepath.Join(os.TempDir(), "wb2a-trae-login-state.json")

	switch args[0] {
	case "url":
		// 设备号与机器号在「生成 URL」这一步就定下来，并写入 state 文件；
		// poll 时从 state 读回，保证「URL 里告知上游」与「落盘写进 auth」的是同一组值。
		dev, mach := strings.TrimSpace(*deviceID), strings.TrimSpace(*machineID)
		u, err := login_trae.StartLocal(client, statePath, dev, mach)
		if err != nil {
			fatal("生成授权 URL 失败: %v", err)
		}
		fmt.Println(u)
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "== 请在浏览器打开上面的地址并完成登录 ==")
		fmt.Fprintln(os.Stderr, "登录后浏览器会跳到一个「打不开的页面」（容器内的 127.0.0.1）；")
		fmt.Fprintln(os.Stderr, "这是正常的 —— 请复制地址栏里的**完整 URL**，然后执行：")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintf(os.Stderr, "  login_trae -auth-dir %s poll '<粘贴完整回调URL>'\n", *authDir)
		fmt.Fprintln(os.Stderr, "")
		if dev == "" {
			fmt.Fprintln(os.Stderr, "⚠️  未提供 --device-id：签到可能因设备校验失败返回 9074。")
			fmt.Fprintln(os.Stderr, "    取值方法：在装过 Trae 客户端的电脑上打开")
			fmt.Fprintln(os.Stderr, `      %APPDATA%\TRAE SOLO CN\User\globalStorage\storage.json`)
			fmt.Fprintln(os.Stderr, `    搜索 "iCubeAuthInfo://icube-dc:" ，后面那串 16 位数字就是设备号。`)
		}

	case "poll":
		if len(args) < 2 {
			fatal("用法: login_trae poll '<完整回调URL>'")
		}
		res, err := login_trae.PollCallback(client, statePath, args[1])
		if err != nil {
			fatal("登录失败: %v", err)
		}
		fp, err := login_trae.SaveAuth(*authDir, res)
		if err != nil {
			fatal("保存凭证失败: %v", err)
		}
		fmt.Printf("登录成功\n  账号：%s（uid=%s）\n  文件：%s\n", res.Nickname, res.UID, fp)
		fmt.Println("  重启服务以加载新账号：docker compose restart workbuddy-wild")

	default:
		fatal("未知子命令 %q（需要 url | poll）", args[0])
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `TraeWork 无头登录助手

用法：
  login_trae [选项] url                              生成授权 URL
  login_trae [选项] poll '<完整回调URL>'             用回调 URL 换取凭证并落盘

选项：
  -auth-dir string     账号文件写入目录（默认 $WB2A_AUTH_DIR 或 /data/auths）
  -device-id string    Trae 客户端真实注册设备号（★ 签到必需）
  -machine-id string   机器号（32 位 hex，可选）
  -timeout duration    HTTP 超时（默认 30s）

设备号取值步骤（★ 不做这步，签到会一直返回 9074）：
  1) 在装过 Trae 客户端的 Windows 电脑上，打开目录：
       %%APPDATA%%\TRAE SOLO CN\User\globalStorage\
  2) 用记事本打开 storage.json，搜索：iCubeAuthInfo://icube-dc:
  3) 冒号后面那串 16 位数字即为设备号，例如 4484256452647802

典型用法（容器内）：
  docker compose run --rm workbuddy-wild /app/login-trae -device-id 4484256452647802 url
  docker compose run --rm workbuddy-wild /app/login-trae poll '<回调URL>'
`)
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "login_trae: "+format+"\n", args...)
	os.Exit(1)
}
