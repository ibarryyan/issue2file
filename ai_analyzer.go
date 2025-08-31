package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-github/v57/github"
	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/openai"
)

// 使用AI生成issues总结
func generateAISummary(issues []*github.Issue, outputDirPath, summaryFile, aiToken, aiModel, aiBaseURL string) error {
	// 准备AI分析的输入数据
	var issuesData strings.Builder
	issuesData.WriteString("以下是GitHub仓库的issues列表，请分析这些issues并提供总结：\n\n")

	// 构建issues表格数据
	issuesData.WriteString("| 编号 | 标题 | 状态 | 创建时间 | 标签 |\n")
	issuesData.WriteString("|------|------|------|----------|------|\n")

	for _, issue := range issues {
		// 获取标签
		var labels []string
		for _, label := range issue.Labels {
			labels = append(labels, label.GetName())
		}
		labelStr := strings.Join(labels, ", ")

		// 添加issue行
		issuesData.WriteString(fmt.Sprintf("| #%d | %s | %s | %s | %s |\n",
			issue.GetNumber(),
			issue.GetTitle(),
			issue.GetState(),
			issue.GetCreatedAt().Format("2006-01-02"),
			labelStr))

		// 添加issue描述（如果有）
		if issue.GetBody() != "" {
			issuesData.WriteString(fmt.Sprintf("\n**Issue #%d 描述**:\n%s\n\n",
				issue.GetNumber(), issue.GetBody()))
		}
	}

	llm, err := openai.New(openai.WithToken(aiToken), openai.WithModel(aiModel), openai.WithBaseURL(aiBaseURL))
	if err != nil {
		return fmt.Errorf("创建AI客户端失败: %w", err)
	}

	completion, err := llms.GenerateFromSinglePrompt(context.Background(), llm, issuesData.String())
	if err != nil {
		return fmt.Errorf("发送AI请求失败: %w", err)
	}

	// 构建总结文件内容
	var summary strings.Builder
	summary.WriteString("# GitHub Issues 分析总结\n\n")
	summary.WriteString("*由AI自动生成*\n\n")
	summary.WriteString("## AI分析\n\n")
	summary.WriteString(completion)
	summary.WriteString("\n\n## Issues列表\n\n")

	// 添加issues表格
	summary.WriteString("| 编号 | 标题 | 状态 | 创建时间 | 标签 |\n")
	summary.WriteString("|------|------|------|----------|------|\n")

	for _, issue := range issues {
		// 获取标签
		var labels []string
		for _, label := range issue.Labels {
			labels = append(labels, label.GetName())
		}
		labelStr := strings.Join(labels, ", ")

		// 添加issue行
		summary.WriteString(fmt.Sprintf("| [#%d](%s) | %s | %s | %s | %s |\n",
			issue.GetNumber(),
			issue.GetHTMLURL(),
			issue.GetTitle(),
			issue.GetState(),
			issue.GetCreatedAt().Format("2006-01-02"),
			labelStr))
	}

	// 写入文件
	summaryPath := filepath.Join(outputDirPath, summaryFile)
	return os.WriteFile(summaryPath, []byte(summary.String()), 0644)
}
