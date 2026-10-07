package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"
)

// Config 表示程序的配置
type Config struct {
	// GitHub API token
	GitHubToken string

	// AI API token
	AIToken string

	// AI Model
	AIModel string

	// AI Base URL
	AIBaseURL string

	// 下面几个开关用 *bool：nil 表示「配置文件没写」，
	// 这样配置文件里缺失的项不会把命令行显式传入的值静默覆盖成 false。
	// 是否下载issue评论
	CommentEnable *bool

	// 是否使用AI分析issues
	AiEnable *bool

	// 是否生成图表
	ChartEnable *bool

	// 是否生成可视化面板（dashboard.html + issues.json）
	DashboardEnable *bool

	// 是否在导出后启动本地 Web 服务
	Serve *bool

	// 是否把 Pull Request 也当作 issue 一起导出
	IncludePR *bool

	// 本地 Web 服务监听端口，<=0 表示未配置
	Port int

	// 指定输出目录
	OutputDir string

	// AI分析总结文件名
	SummaryFile string
}

// LoadConfig 从指定路径加载配置文件。
// 任何失败都以 error 返回，由调用方决定是否终止程序，不在库函数中 panic。
func LoadConfig(filePath string) (*Config, error) {
	if filePath == "" {
		return nil, errors.New("配置文件路径为空")
	}
	if _, err := os.Stat(filePath); err != nil {
		return nil, fmt.Errorf("配置文件不可访问: %w", err)
	}

	conf := viper.New()
	// 交给 viper 自己解析目录与文件名，避免手写 strings.Split 漏掉路径分隔符
	conf.SetConfigFile(filePath)
	conf.SetConfigType("toml")
	if err := conf.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("读取配置文件 %s 失败: %w", filePath, err)
	}

	return &Config{
		GitHubToken:     conf.GetString("gitHubToken"),
		AIToken:         conf.GetString("aiToken"),
		AIModel:         conf.GetString("aiModel"),
		AIBaseURL:       conf.GetString("aiBaseURL"),
		CommentEnable:   boolPtr(conf, "commentEnable"),
		AiEnable:        boolPtr(conf, "aiEnable"),
		ChartEnable:     boolPtr(conf, "chartEnable"),
		DashboardEnable: boolPtr(conf, "dashboardEnable"),
		Serve:           boolPtr(conf, "serve"),
		IncludePR:       boolPtr(conf, "includePR"),
		Port:            conf.GetInt("port"),
		OutputDir:       conf.GetString("outputDir"),
		SummaryFile:     conf.GetString("summaryFile"),
	}, nil
}

// boolPtr 仅在配置文件显式声明了该键时才返回值，否则返回 nil。
func boolPtr(conf *viper.Viper, key string) *bool {
	if !conf.IsSet(key) {
		return nil
	}
	v := conf.GetBool(key)
	return &v
}

// String 返回脱敏后的配置摘要，避免 token 被打印到日志或终端。
func (c *Config) String() string {
	mask := func(s string) string {
		if s == "" {
			return "<未设置>"
		}
		if len(s) <= 8 {
			return "****"
		}
		return s[:4] + "****" + s[len(s)-4:]
	}
	flag := func(p *bool) string {
		if p == nil {
			return "<默认>"
		}
		return fmt.Sprintf("%v", *p)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "gitHubToken=%s aiToken=%s aiModel=%s", mask(c.GitHubToken), mask(c.AIToken), c.AIModel)
	fmt.Fprintf(&sb, " comment=%s ai=%s chart=%s dashboard=%s includePR=%s",
		flag(c.CommentEnable), flag(c.AiEnable), flag(c.ChartEnable),
		flag(c.DashboardEnable), flag(c.IncludePR))
	fmt.Fprintf(&sb, " serve=%s port=%d outputDir=%s summaryFile=%s",
		flag(c.Serve), c.Port, c.OutputDir, c.SummaryFile)
	return sb.String()
}
