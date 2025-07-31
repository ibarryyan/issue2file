package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-github/v57/github"
)

// AI分析请求结构
type AIAnalysisRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
}

// 消息结构
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// AI分析响应结构
type AIAnalysisResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// 使用AI生成issues总结
func generateAISummary(issues []*github.Issue, outputDirPath, summaryFile, aiToken string) error {
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

	// 构建AI请求
	request := AIAnalysisRequest{
		Model: AIModelDeepSeek,
		Messages: []Message{
			{
				Role:    "user",
				Content: issuesData.String(),
			},
		},
	}

	// 将请求转换为JSON
	requestBody, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("构建AI请求失败: %w", err)
	}

	// 创建HTTP请求
	req, err := http.NewRequest("POST", AIDeepSeekUrl, bytes.NewBuffer(requestBody))
	if err != nil {
		return fmt.Errorf("创建HTTP请求失败: %w", err)
	}

	// 设置请求头
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+aiToken)

	// 发送请求
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("发送AI请求失败: %w", err)
	}
	defer resp.Body.Close()

	// 读取响应
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读取AI响应失败: %w", err)
	}

	// 检查响应状态
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("AI API返回错误: %s, %s", resp.Status, string(respBody))
	}

	// 解析响应
	var aiResp AIAnalysisResponse
	if err := json.Unmarshal(respBody, &aiResp); err != nil {
		return fmt.Errorf("解析AI响应失败: %w", err)
	}

	// 检查是否有内容
	if len(aiResp.Choices) == 0 || aiResp.Choices[0].Message.Content == "" {
		return fmt.Errorf("AI响应没有内容")
	}

	// 构建总结文件内容
	var summary strings.Builder
	summary.WriteString("# GitHub Issues 分析总结\n\n")
	summary.WriteString("*由AI自动生成*\n\n")
	summary.WriteString("## AI分析\n\n")
	summary.WriteString(aiResp.Choices[0].Message.Content)
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
