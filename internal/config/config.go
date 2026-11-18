// Package config 解析并校验环境变量（DESIGN.md 9.1）。唯一配置入口。
package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config 汇总全部运行配置。字段与 DESIGN.md 9.1 一一对应。
type Config struct {
	APIToken            string        // MAILHUB_API_TOKEN（必填）
	SecretKey           []byte        // MAILHUB_SECRET_KEY（必填，base64 的 32 字节）
	Listen              string        // MAILHUB_LISTEN
	DataDir             string        // MAILHUB_DATA_DIR
	DatabaseURL         string        // MAILHUB_DATABASE_URL（空 = SQLite）
	MaxMessageSize      int64         // MAILHUB_MAX_MESSAGE_SIZE（字节）
	RetentionDays       int           // MAILHUB_RETENTION_DAYS（0 = 永久）
	InitialSyncDays     int           // MAILHUB_INITIAL_SYNC_DAYS（首次同步窗口，默认 90 天）
	IdleBudgetPerHost   int           // MAILHUB_IDLE_BUDGET_PER_HOST（0 = 不限；M2 起生效）
	ConnectStagger      time.Duration // MAILHUB_CONNECT_STAGGER
	PollIntervalDefault int           // MAILHUB_POLL_INTERVAL_DEFAULT（秒）
	FallbackInterval    int           // MAILHUB_FALLBACK_INTERVAL（秒；M2 起生效）
	WebhookConcurrency  int           // MAILHUB_WEBHOOK_CONCURRENCY
}

// Load 从环境变量构建配置并校验。genSecret 为 true 时仅生成并打印一个
// 随机 MAILHUB_SECRET_KEY 后退出（方便首次部署，见 main.go）。
func Load() (*Config, error) {
	c := &Config{
		APIToken:            os.Getenv("MAILHUB_API_TOKEN"),
		SecretKey:           nil,
		Listen:              getenv("MAILHUB_LISTEN", ":8080"),
		DataDir:             getenv("MAILHUB_DATA_DIR", "./data"),
		DatabaseURL:         os.Getenv("MAILHUB_DATABASE_URL"),
		MaxMessageSize:      50 << 20,
		RetentionDays:       getenvInt("MAILHUB_RETENTION_DAYS", 0),
		InitialSyncDays:     getenvInt("MAILHUB_INITIAL_SYNC_DAYS", 90),
		IdleBudgetPerHost:   getenvInt("MAILHUB_IDLE_BUDGET_PER_HOST", 0),
		ConnectStagger:      getenvDur("MAILHUB_CONNECT_STAGGER", 2*time.Second),
		PollIntervalDefault: getenvInt("MAILHUB_POLL_INTERVAL_DEFAULT", 60),
		FallbackInterval:    getenvInt("MAILHUB_FALLBACK_INTERVAL", 300),
		WebhookConcurrency:  getenvInt("MAILHUB_WEBHOOK_CONCURRENCY", 4),
	}
	if v := os.Getenv("MAILHUB_MAX_MESSAGE_SIZE"); v != "" {
		n, err := parseSize(v)
		if err != nil {
			return nil, fmt.Errorf("MAILHUB_MAX_MESSAGE_SIZE: %w", err)
		}
		c.MaxMessageSize = n
	}

	if c.APIToken == "" {
		return nil, errors.New("MAILHUB_API_TOKEN 必填（API 访问令牌）")
	}
	key, err := base64.StdEncoding.DecodeString(os.Getenv("MAILHUB_SECRET_KEY"))
	if err != nil {
		return nil, errors.New("MAILHUB_SECRET_KEY 不是合法 base64")
	}
	if len(key) != 32 {
		return nil, errors.New("MAILHUB_SECRET_KEY 必须是 base64 编码的 32 字节（可用 mailhub --gen-secret 生成）")
	}
	c.SecretKey = key

	if c.PollIntervalDefault < 5 {
		return nil, errors.New("MAILHUB_POLL_INTERVAL_DEFAULT 不能小于 5 秒")
	}
	if c.InitialSyncDays <= 0 {
		return nil, errors.New("MAILHUB_INITIAL_SYNC_DAYS 必须大于 0")
	}
	if c.WebhookConcurrency < 1 {
		c.WebhookConcurrency = 1
	}
	return c, nil
}

// GenSecret 打印一个随机 32 字节主密钥（base64）。
func GenSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func getenvInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getenvDur(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// parseSize 接受 "50MB"、"10MiB"、"102400" 形式。
func parseSize(s string) (int64, error) {
	suffixes := []struct {
		suf string
		mul int64
	}{
		{"MB", 1 << 20}, {"MiB", 1 << 20},
		{"KB", 1 << 10}, {"KiB", 1 << 10},
		{"GB", 1 << 30}, {"GiB", 1 << 30},
	}
	for _, sfx := range suffixes {
		n := len(s) - len(sfx.suf)
		if n > 0 && s[n:] == sfx.suf {
			v, err := strconv.ParseInt(s[:n], 10, 64)
			if err != nil {
				return 0, err
			}
			return v * sfx.mul, nil
		}
	}
	return strconv.ParseInt(s, 10, 64)
}
