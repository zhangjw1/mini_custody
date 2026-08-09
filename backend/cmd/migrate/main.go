package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
	"github.com/xiaoqi/mini-custody/backend/migrations"
)

// main 是数据库迁移工具入口。
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "数据库迁移工具执行失败：", err)
		os.Exit(1)
	}
}

// run 执行数据库升级、回退、版本查询或结构检查。
func run(args []string) error {
	if len(args) != 1 {
		return errors.New("用法：migrate <up|down|version|check>")
	}
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return errors.New("必须配置 DATABASE_URL")
	}
	timezoneName := strings.TrimSpace(os.Getenv("APP_TIMEZONE"))
	if timezoneName == "" {
		timezoneName = "Asia/Shanghai"
	}
	timezone, err := time.LoadLocation(timezoneName)
	if err != nil {
		return errors.New("APP_TIMEZONE 配置无效")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := postgres.Open(ctx, databaseURL, timezone)
	if err != nil {
		return err
	}
	defer pool.Close()

	runner := migrations.NewRunner(pool)
	switch args[0] {
	case "up":
		return runner.Up(ctx)
	case "down":
		return runner.Down(ctx)
	case "version":
		version, err := runner.Version(ctx)
		if err != nil {
			return err
		}
		fmt.Println(version)
		return nil
	case "check":
		result, err := migrations.VerifySchema(ctx, pool)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	default:
		return errors.New("用法：migrate <up|down|version|check>")
	}
}
