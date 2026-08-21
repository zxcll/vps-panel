// Package config 负责面板的引导配置。
//
// 这里只放启动期必须知道的东西（监听地址、数据目录、主密钥）。
// 运行期可调的参数（离线判定阈值、切换冷却等）放在数据库 settings 表里，
// 这样用户能在面板上直接改，不用重启进程。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	// Listen 是面板 HTTP 监听地址。默认只绑本机，建议前面挂 Nginx 反代 + HTTPS。
	Listen string `yaml:"listen"`
	// DataDir 存放 SQLite 数据库和主密钥文件。
	DataDir string `yaml:"data_dir"`
	// BaseURL 是探针能访问到的面板地址，用于生成一键安装脚本里的 --server 参数。
	// 例如 https://panel.example.com
	BaseURL string `yaml:"base_url"`
	// MasterKey 是加密 SSH / DNS 凭据的主密钥（64 位十六进制 = 32 字节）。
	// 留空则从 PANEL_MASTER_KEY 环境变量读，再空则在 DataDir 下生成并持久化。
	MasterKey string `yaml:"master_key"`
	// TrustProxyHeaders 为 true 时从 X-Forwarded-For 取客户端 IP（面板在反代后面时开）。
	TrustProxyHeaders bool `yaml:"trust_proxy_headers"`
}

func Default() Config {
	return Config{
		Listen:  "127.0.0.1:8080",
		DataDir: "./data",
		BaseURL: "",
	}
}

// Load 读取 yaml 配置；path 为空或文件不存在时返回默认值（首次运行的场景）。
// 环境变量优先级高于文件。
func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := yaml.Unmarshal(b, &cfg); err != nil {
				return cfg, fmt.Errorf("解析配置文件 %s: %w", path, err)
			}
		case errors.Is(err, os.ErrNotExist):
			// 允许无配置文件启动，全部走默认值 + 环境变量
		default:
			return cfg, fmt.Errorf("读取配置文件 %s: %w", path, err)
		}
	}

	if v := os.Getenv("PANEL_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("PANEL_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	if v := os.Getenv("PANEL_BASE_URL"); v != "" {
		cfg.BaseURL = v
	}
	if v := os.Getenv("PANEL_MASTER_KEY"); v != "" {
		cfg.MasterKey = v
	}
	if v := os.Getenv("PANEL_TRUST_PROXY"); v != "" {
		cfg.TrustProxyHeaders = v == "1" || strings.EqualFold(v, "true")
	}

	cfg.BaseURL = NormalizeBaseURL(cfg.BaseURL)

	abs, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return cfg, fmt.Errorf("解析数据目录: %w", err)
	}
	cfg.DataDir = abs

	return cfg, nil
}

// NormalizeBaseURL 补全面板对外地址的协议前缀。
//
// 用户很容易只填 "1.2.3.4:8080"。这个值会原样写进探针的配置，
// 而探针拿到不带协议的地址只能靠猜——猜成 https 就会撞上
// "server gave HTTP response to HTTPS client"。在源头补齐最省事。
//
// 补什么不靠端口号猜：面板进程只提供明文 HTTP，从来不自己终结 TLS。
// 想走 HTTPS 必然是前面挂了 Nginx 之类的反代，那种情况用户会把
// https://域名 写全。所以没写协议时一律补 http://。
func NormalizeBaseURL(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimRight(s, "/")
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return s
	}
	return "http://" + s
}

// AgentsDir 是探针二进制的存放目录，面板通过 /agent/download 分发给节点。
func (c Config) AgentsDir() string { return filepath.Join(c.DataDir, "agents") }

func (c Config) DBPath() string      { return filepath.Join(c.DataDir, "panel.db") }
func (c Config) KeyFilePath() string { return filepath.Join(c.DataDir, "master.key") }

// ResolveMasterKey 返回 32 字节主密钥。
// 优先用配置/环境变量里的值；都没有就在数据目录生成一个 0600 权限的密钥文件。
//
// 注意：这个密钥丢了，库里所有 SSH 密码和 DNS 凭据都解不开，只能重填。
func (c Config) ResolveMasterKey() ([]byte, error) {
	if c.MasterKey != "" {
		key, err := hex.DecodeString(strings.TrimSpace(c.MasterKey))
		if err != nil {
			return nil, fmt.Errorf("主密钥不是合法的十六进制字符串: %w", err)
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("主密钥必须是 32 字节（64 位十六进制），当前 %d 字节", len(key))
		}
		return key, nil
	}

	path := c.KeyFilePath()
	b, err := os.ReadFile(path)
	if err == nil {
		key, decErr := hex.DecodeString(strings.TrimSpace(string(b)))
		if decErr != nil || len(key) != 32 {
			return nil, fmt.Errorf("主密钥文件 %s 内容损坏，请修复或删除后重启（删除会导致已存凭据无法解密）", path)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("读取主密钥文件: %w", err)
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("生成主密钥: %w", err)
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(key)), 0o600); err != nil {
		return nil, fmt.Errorf("写入主密钥文件: %w", err)
	}
	return key, nil
}
