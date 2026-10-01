// Команда okdesk — модуль интеграции Asterisk 16 с Okdesk
// (screen-pop, журналирование звонков, автопривязка к заявке).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/ovn25519/ast2okdesk/internal/ami"
	"github.com/ovn25519/ast2okdesk/internal/callflow"
	"github.com/ovn25519/ast2okdesk/internal/cleanup"
	"github.com/ovn25519/ast2okdesk/internal/config"
	"github.com/ovn25519/ast2okdesk/internal/journal"
	"github.com/ovn25519/ast2okdesk/internal/monitor"
	"github.com/ovn25519/ast2okdesk/internal/okdesk"
	"github.com/ovn25519/ast2okdesk/internal/recording"
	"github.com/ovn25519/ast2okdesk/internal/retry"
	"github.com/ovn25519/ast2okdesk/internal/store"
)

// version и commit подставляются при сборке:
//
//	go build -ldflags "-X main.version=v0.1.0 -X main.commit=abc1234"
var (
	version = "dev"
	commit  = "unknown"
)

// shutdownTimeout — предел ожидания остановки компонентов.
const shutdownTimeout = 15 * time.Second

func main() {
	os.Exit(run())
}

func run() int {
	configPath := flag.String("config", "config.toml", "путь к файлу конфигурации")
	showVersion := flag.Bool("version", false, "показать версию и выйти")
	flag.Parse()

	if *showVersion {
		fmt.Printf("okdesk %s (commit %s)\n", version, commit)
		return 0
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("не удалось загрузить конфигурацию", "error", err)
		return 1
	}
	// cfg реализует slog.LogValuer: секреты в лог не попадают.
	logger.Info("конфигурация загружена", "version", version, "commit", commit, "cfg", cfg)

	asteriskLoc, err := time.LoadLocation(cfg.Asterisk.Timezone)
	if err != nil {
		logger.Error("некорректная зона Asterisk", "timezone", cfg.Asterisk.Timezone, "error", err)
		return 1
	}
	okdeskLoc, err := time.LoadLocation(cfg.Okdesk.Timezone)
	if err != nil {
		logger.Error("некорректная зона Okdesk", "timezone", cfg.Okdesk.Timezone, "error", err)
		return 1
	}

	dbPath := filepath.Join(filepath.Dir(*configPath), "okdesk.db")
	st, err := store.Open(dbPath)
	if err != nil {
		logger.Error("не удалось открыть хранилище", "path", dbPath, "error", err)
		return 1
	}
	defer func() {
		if err := st.Close(); err != nil {
			logger.Error("закрытие хранилища", "error", err)
		}
	}()

	counters := &monitor.Counters{}
	okClient, err := okdesk.New(okdesk.Config{
		BaseURL:            cfg.Okdesk.BaseURL,
		APIToken:           cfg.Okdesk.APIToken,
		SearchNumbersCount: cfg.Okdesk.SearchNumbersCount,
		Timezone:           okdeskLoc,
		Logger:             logger,
		Observer:           counters,
	})
	if err != nil {
		logger.Error("не удалось создать клиент Okdesk", "error", err)
		return 1
	}

	loc := recording.New(cfg.Recordings.BaseURL, asteriskLoc)

	employees := make(map[string]int, len(cfg.Employees))
	for _, e := range cfg.Employees {
		employees[e.SIPPeer] = e.OkdeskTelephonyNumber
	}

	dispatcher := callflow.New(st, okClient, callflow.Config{
		Queue:           cfg.Asterisk.Queue,
		TelephonyNumber: cfg.Okdesk.TelephonyNumber,
		Employees:       employees,
	}, logger)

	finalizer := journal.New(st, okClient, loc, journal.Config{
		IncomingPhoneNumber: cfg.Okdesk.IncomingPhoneNumber,
		RetryInitialBackoff: time.Duration(cfg.Retry.InitialBackoffSeconds) * time.Second,
	}, logger)
	dispatcher.SetFinalizer(finalizer)

	amiClient := ami.New(ami.Options{
		Address:  net.JoinHostPort(cfg.Asterisk.AMI.Host, strconv.Itoa(cfg.Asterisk.AMI.Port)),
		Username: cfg.Asterisk.AMI.User,
		Secret:   cfg.Asterisk.AMI.Password,
		Logger:   logger,
	})

	retryWorker := retry.New(st, okClient, retry.Config{
		MaxAttempts:    cfg.Retry.MaxAttempts,
		InitialBackoff: time.Duration(cfg.Retry.InitialBackoffSeconds) * time.Second,
		MaxBackoff:     time.Duration(cfg.Retry.MaxBackoffSeconds) * time.Second,
	}, logger)

	cleaner := cleanup.New(st, cleanup.Config{
		TTL:      time.Duration(cfg.Retention.CorrelationTTLHours) * time.Hour,
		Interval: time.Duration(cfg.Retention.CleanupIntervalMinutes) * time.Minute,
	}, logger)

	mon := monitor.New(monitor.Config{
		FilesDir: cfg.Recordings.FilesDir,
	}, counters, amiClient, st, finalizer, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	launch := func(name string, fn func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(ctx); err != nil {
				logger.Error("компонент остановлен с ошибкой", "component", name, "error", err)
			}
		}()
	}

	launch("ami", amiClient.Run)
	launch("dispatcher", func(c context.Context) error { return dispatcher.Run(c, amiClient.Events()) })
	launch("retry", retryWorker.Run)
	launch("cleanup", cleaner.Run)
	launch("monitor", mon.Run)

	logger.Info("сервис запущен",
		"queue", cfg.Asterisk.Queue, "ami", amiClient.Addr(), "employees", len(employees))
	<-ctx.Done()
	logger.Info("получен сигнал завершения, останавливаемся")

	stopped := make(chan struct{})
	go func() {
		wg.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(shutdownTimeout):
		logger.Warn("таймаут ожидания остановки компонентов")
	}

	dispatcher.Wait()
	finalizer.Close()

	logger.Info("сервис остановлен")
	return 0
}
