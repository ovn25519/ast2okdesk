// Команда okdesk — модуль интеграции Asterisk 16 с Okdesk
// (screen-pop, журналирование звонков, автопривязка к заявке).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ovn25519/ast2okdesk/internal/config"
)

// version и commit подставляются при сборке:
//
//	go build -ldflags "-X main.version=v0.1.0 -X main.commit=abc1234"
var (
	version = "dev"
	commit  = "unknown"
)

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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// На последующих этапах здесь будет запуск AMI-клиента, диспетчера
	// событий, retry-воркера и фонового джоба чистки SQLite.
	logger.Info("сервис запущен, ожидание сигнала завершения")
	<-ctx.Done()
	logger.Info("получен сигнал завершения, останавливаемся")

	return 0
}
