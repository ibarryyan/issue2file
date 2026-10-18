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

// 默认 AI 配置，与 main.go 中的 flag 默认值保持一致。
//
// 注意：这里的 baseURL 是"前缀"而非完整 endpoint —— langchaingo 会自己在后面
// 拼上 /chat/completions（见 llms/openai/internal/openaiclient 的 buildURL）。
// 写成 .../v1/chat/completions 会导致实际请求路径变成
// /v1/chat/completions/chat/completions 而必然 404。
const (
	defaultAIModel   = "deepseek-chat"
	defaultAIBaseURL = "https://api.deepseek.com/v1"
)

// normalizeAIBaseURL 容错处理用户配置的地址：既允许填 https://host/v1，
// 也允许填 .../v1/chat/completions（旧版本的默认值），统一收敛成"前缀"形式。
func normalizeAIBaseURL(baseURL string) string {
	u := strings.TrimSpace(baseURL)
	if u == "" {
		return ""
	}
	u = strings.TrimRight(u, "/")
	u = strings.TrimSuffix(u, "/chat/completions")
	return strings.TrimRight(u, "/")
}

// newLLMClient 创建 OpenAI 兼容的 LLM 客户端；model / baseURL 为空时回落默认值。
func newLLMClient(token, model, baseURL string) (llms.Model, error) {
	if model == "" {
		model = defaultAIModel
	}
	if baseURL == "" {
		baseURL = defaultAIBaseURL
	} else {
		baseURL = normalizeAIBaseURL(baseURL)
	}
	llm, err := openai.New(openai.WithToken(token), openai.WithModel(model), openai.WithBaseURL(baseURL))
	if err != nil {
		return nil, fmt.Errorf("创建AI客户端失败: %w", err)
	}
	return llm, nil
}

// generateFromPrompt 统一封装单次问答，便于测试替换与统一超时。
func generateFromPrompt(ctx context.Context, llm llms.Model, prompt string) (string, error) {
	return llms.GenerateFromSinglePrompt(ctx, llm, prompt)
}

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

	llm, err := newLLMClient(aiToken, aiModel, aiBaseURL)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), aiTimeout)
	defer cancel()

	completion, err := generateFromPrompt(ctx, llm, issuesData.String())
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
