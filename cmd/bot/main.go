package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/fsykk/new-api-bot/internal/bot"
	"github.com/fsykk/new-api-bot/internal/config"
	"github.com/fsykk/new-api-bot/internal/health"
	"github.com/fsykk/new-api-bot/internal/mailer"
	"github.com/fsykk/new-api-bot/internal/newapi"
	"github.com/fsykk/new-api-bot/internal/qq"
	"github.com/fsykk/new-api-bot/internal/secure"
	"github.com/fsykk/new-api-bot/internal/store"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("配置校验失败", "error", err)
		os.Exit(2)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)
	if len(os.Args) > 1 {
		handled, err := runMaintenance(cfg, os.Args[1:])
		if err != nil {
			logger.Error("维护命令执行失败", "error", err)
			os.Exit(1)
		}
		if handled {
			return
		}
		logger.Error("未知启动参数")
		os.Exit(2)
	}

	storage, err := store.Open(cfg.DataPath)
	if err != nil {
		logger.Error("打开本地数据库失败", "error", err)
		os.Exit(1)
	}
	defer storage.Close()
	box, err := secure.New(cfg.BotDataKey)
	if err != nil {
		logger.Error("初始化本地加密失败", "error", err)
		os.Exit(1)
	}

	newAPIClient := newapi.New(cfg.NewAPIBaseURL, cfg.NewAPIAdminToken, cfg.NewAPIAdminUserID, cfg.NewAPITimeout)
	qqClient := qq.NewClient(cfg.QQAppID, cfg.QQAppSecret, cfg.QQAPITimeout)
	smtpSender := mailer.NewSMTP(cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPUsername, cfg.SMTPPassword, cfg.SMTPFrom, cfg.SMTPTLSMode, cfg.NewAPITimeout)
	gateway := qq.NewGateway(qqClient, storage, logger)
	service := bot.New(cfg, storage, box, newAPIClient, qqClient, smtpSender, logger)
	service.SetGatewayConnectedFunc(gateway.Connected)

	preflightCtx, preflightCancel := context.WithTimeout(context.Background(), cfg.NewAPITimeout)
	if status, err := newAPIClient.GetStatus(preflightCtx, true); err != nil {
		logger.Warn("New API 启动预检失败，服务将继续启动并在后台重试", "error", err)
	} else {
		logger.Info("New API 连接成功", "system", status.SystemName, "version", status.Version, "quota_per_unit", status.QuotaPerUnit)
	}
	preflightCancel()

	appCtx, stopSignal := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignal()
	serviceCtx, cancelService := context.WithCancel(context.Background())
	service.Start(serviceCtx)

	gatewayDone := make(chan struct{})
	go func() {
		defer close(gatewayDone)
		if err := gateway.Run(appCtx, service.HandleGateway); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("QQ Gateway 已停止", "error", err)
			stopSignal()
		}
	}()

	healthServer := &health.Server{Store: storage, NewAPI: newAPIClient, QQ: qqClient, Gateway: gateway}
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           healthServer.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	httpDone := make(chan struct{})
	go func() {
		defer close(httpDone)
		logger.Info("健康检查服务已启动", "listen", cfg.ListenAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("健康检查服务异常退出", "error", err)
			stopSignal()
		}
	}()

	<-appCtx.Done()
	logger.Info("收到退出信号，开始优雅关闭")
	<-gatewayDone
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := service.StopContext(shutdownCtx); err != nil {
		logger.Warn("命令队列未在宽限期内完成，取消剩余任务", "error", err)
		cancelService()
		forceCtx, forceCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if forceErr := service.StopContext(forceCtx); forceErr != nil {
			logger.Error("取消任务后服务仍未及时停止", "error", forceErr)
		}
		forceCancel()
	} else {
		cancelService()
	}
	shutdownCancel()
	httpShutdownCtx, httpShutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := httpServer.Shutdown(httpShutdownCtx); err != nil {
		logger.Warn("健康检查服务优雅停止超时，执行强制关闭", "error", err)
		_ = httpServer.Close()
	}
	httpShutdownCancel()
	<-httpDone
	logger.Info("服务已停止")
}
