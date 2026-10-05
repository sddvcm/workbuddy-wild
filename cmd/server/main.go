// main.go WorkBuddy-Wild 无头服务入口（Linux/Docker 部署用）。
//
// 双平台架构（与桌面端 main.go 对齐）：
//   - workbuddy 与 traework 各自独立 pool + state 文件 + scheduler
//   - 一个 HTTP 服务按 model 前缀路由（workbuddy/* 或 traework/*）
//
// 配置来源优先级：-config 指定的 JSON 文件 → WB2A_* 环境变量覆盖 → 内置默认值。
// 文件不存在时回退「默认值 + 环境变量」，便于容器只配环境变量。
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/auth"
	"github.com/rockswang/workbuddy-wild/internal/config"
	"github.com/rockswang/workbuddy-wild/internal/pool"
	"github.com/rockswang/workbuddy-wild/internal/provider"
	"github.com/rockswang/workbuddy-wild/internal/scheduler"
	"github.com/rockswang/workbuddy-wild/internal/server"
	"github.com/rockswang/workbuddy-wild/internal/traework"
	"github.com/rockswang/workbuddy-wild/internal/upstream"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	log.Printf("config: listen=%s auth_dir=%s state_file=%s region=%s strategy=%s max_rotate=%d",
		cfg.Listen.Addr(), cfg.AuthDir, cfg.StateFile, cfg.Region, cfg.Strategy, cfg.MaxRotate)

	// 账号目录与状态目录必须先存在，否则 LoadDir 会拿到空结果、pool 无法落盘。
	_ = os.MkdirAll(cfg.AuthDir, 0o755)
	stateDir := filepath.Dir(cfg.StateFile)
	_ = os.MkdirAll(stateDir, 0o755)

	// ── 双平台账号加载 ────────────────────────────────────────────────
	// workbuddy 需按 region 过滤；traework 只有 CN，不过滤。
	wbAuths, err := auth.LoadWorkBuddyDir(cfg.AuthDir, cfg.Region)
	if err != nil {
		log.Fatalf("load workbuddy auths: %v", err)
	}
	trAuths, err := auth.LoadTraeDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load traework auths: %v", err)
	}
	log.Printf("loaded accounts: workbuddy=%d %s, traework=%d from %s",
		len(wbAuths), cfg.Region, len(trAuths), cfg.AuthDir)
	if len(wbAuths) == 0 && len(trAuths) == 0 {
		log.Printf("⚠️  没有任何账号：请先用 docker/login.sh 登录，或把明文 auth 文件放进 %s", cfg.AuthDir)
	}

	// ── 双平台账号池（状态文件互相隔离，冷却互不影响）─────────────────
	wbPool := pool.New(filepath.Join(stateDir, "state-workbuddy.json"))
	for _, a := range wbAuths {
		wbPool.Add(a)
	}
	trPool := pool.New(filepath.Join(stateDir, "state-traework.json"))
	for _, a := range trAuths {
		trPool.Add(a)
	}
	// 两个平台共用同一个选号策略。
	if strat, ok := pool.ParseStrategy(cfg.Strategy); ok {
		wbPool.SetStrategy(strat)
		trPool.SetStrategy(strat)
		log.Printf("选号策略：%s（%s）", strat, strat.Label())
	}

	// ── 双平台上游客户端 ──────────────────────────────────────────────
	wbUp := upstream.New()
	wbUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	trUp := traework.New()
	trUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second

	// ── 双平台调度器（定时签到 + 保活）────────────────────────────────
	checkinMinutes, err := config.ParseClockTimes(cfg.Schedule.CheckinTimes)
	if err != nil {
		log.Fatalf("parse checkin_times: %v", err)
	}
	log.Printf("签到时间点：%v  保活小时：%v", cfg.Schedule.CheckinTimes, cfg.Schedule.KeepaliveHours)

	wbSch := scheduler.New(scheduler.Config{
		Pool: wbPool, Upstream: wbUp, Name: "workbuddy",
		CheckinMinutes: checkinMinutes, KeepaliveHours: cfg.Schedule.KeepaliveHours,
	})
	trSch := scheduler.New(scheduler.Config{
		Pool: trPool, Upstream: trUp, Name: "traework",
		CheckinMinutes: checkinMinutes, KeepaliveHours: cfg.Schedule.KeepaliveHours,
	})

	runtimes := map[provider.Kind]*server.Runtime{
		provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: wbPool, Upstream: wbUp, StaticModels: server.WorkBuddyStaticModels()},
		provider.TraeWork:  {Kind: provider.TraeWork, Pool: trPool, Upstream: trUp, StaticModels: server.TraeWorkStaticModels()},
	}

	h := server.NewHandler(server.Config{
		Runtimes:     runtimes,
		APIKey:       cfg.APIKey,
		MaxRotate:    cfg.MaxRotate,
		HardCooldown: cfg.HardCreditDur,
		SoftCooldown: cfg.SoftRateDur,
		ErrThreshold: cfg.Cooldown.ErrThresh,
		ErrCooldown:  cfg.ErrCooldownDur,
	})

	// ── 启动 ─────────────────────────────────────────────────────────
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go wbSch.Run(ctx)
	go trSch.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen.Addr(),
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	apiKeySet := cfg.APIKey != ""
	if !apiKeySet {
		log.Printf("⚠️  API_KEY 为空：接口无鉴权，仅限本机/内网使用，切勿直接暴露公网")
	}
	log.Printf("workbuddy-wild listening on %s (api_key=%v)", cfg.Listen.Addr(), apiKeySet)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

// loadConfig 读配置：文件不存在时退回「内置默认 + WB2A_* 环境变量」。
func loadConfig(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err == nil {
		return cfg, nil
	}
	if path == "" || os.IsNotExist(err) {
		log.Printf("config %s not found, using defaults + env", path)
		return config.Load("")
	}
	return nil, err
}
