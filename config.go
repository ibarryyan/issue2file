package main

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

// Config 表示程序的配置
type Config struct {
	// GitHub API token
	GitHubToken string

	// AI API token
	AIToken string

	// 是否下载issue评论
	CommentEnable bool

	// 是否使用AI分析issues
	AiEnable bool

	// 是否生成图表
	ChartEnable bool

	// 指定输出目录
	OutputDir string

	// AI分析总结文件名
	SummaryFile string
}

// LoadConfig 从指定路径加载配置文件
func LoadConfig(filePath string) (*Config, error) {

	conf := viper.New()
	split := strings.Split(filePath, "/")
	if len(split) < 2 {
		panic("config err")
	}

	var path string
	for i := 0; i < len(split)-1; i++ {
		path += split[i]
	}

	conf.SetConfigName(split[len(split)-1])
	conf.SetConfigType("toml")
	conf.AddConfigPath(path)
	if err := conf.ReadInConfig(); err != nil {
		panic(fmt.Errorf("fatal error config file: %w", err))
	}

	return &Config{
		GitHubToken:   conf.GetString("gitHubToken"),
		AIToken:       conf.GetString("aiToken"),
		CommentEnable: conf.GetBool("commentEnable"),
		AiEnable:      conf.GetBool("aiEnable"),
		ChartEnable:   conf.GetBool("chartEnable"),
		OutputDir:     conf.GetString("outputDir"),
		SummaryFile:   conf.GetString("summaryFile"),
	}, nil
}
