package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-github/v57/github"
	"golang.org/x/oauth2"
)

func main() {
	// 定义命令行参数
	var (
		token         = flag.String("token", "", "GitHub API token")
		withComments  = flag.Bool("comments", true, "是否下载issue评论")
		withAI        = flag.Bool("ai-summary", false, "是否使用AI分析issues")
		aiToken       = flag.String("ai-token", "", "AI API token")
		outputDirPath = flag.String("output", "", "指定输出目录")
		summaryFile   = flag.String("summary-file", "summary.md", "AI分析总结文件名")
	)

	// 解析命令行参数
	flag.Parse()

	// 检查是否提供了仓库参数
	args := flag.Args()
	if len(args) < 1 {
		fmt.Println("使用方法: issue2file [选项] <仓库地址>")
		fmt.Println("选项:")
		flag.PrintDefaults()
		fmt.Println("\n示例:")
		fmt.Println("  issue2file .                    # 从当前目录的git仓库获取issues")
		fmt.Println("  issue2file -token=xxx owner/repo # 使用token从指定仓库获取issues")
		fmt.Println("  issue2file -ai-summary -ai-token=xxx owner/repo # 使用AI分析issues")
		os.Exit(1)
	}

	repoArg := args[0]

	var owner, repo string
	var err error

	if repoArg == "." {
		// 从当前目录的.git/config读取仓库信息
		owner, repo, err = getRepoFromGitConfig()
		if err != nil {
			log.Fatalf("无法从.git/config获取仓库信息: %v", err)
		}
	} else {
		// 解析仓库地址
		owner, repo, err = parseRepoURL(repoArg)
		if err != nil {
			log.Fatalf("无法解析仓库地址: %v", err)
		}
	}

	fmt.Printf("正在获取仓库 %s/%s 的issues...\n", owner, repo)

	// 创建GitHub客户端
	client := createGitHubClient(*token)

	// 获取issues
	issues, err := fetchIssues(client, owner, repo)
	if err != nil {
		log.Fatalf("获取issues失败: %v", err)
	}

	// 创建输出目录
	var outputDir string
	if *outputDirPath != "" {
		outputDir = *outputDirPath
	} else {
		outputDir = fmt.Sprintf("issues_%s_%s", owner, repo)
	}
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		log.Fatalf("创建输出目录失败: %v", err)
	}

	// 保存issues为Markdown文件
	for _, issue := range issues {
		if err := saveIssueAsMarkdown(issue, outputDir, owner, repo, client, *withComments); err != nil {
			log.Printf("保存issue #%d 失败: %v", issue.GetNumber(), err)
		} else {
			fmt.Printf("已保存 issue #%d: %s\n", issue.GetNumber(), issue.GetTitle())
		}
	}

	fmt.Printf("完成！共保存了 %d 个issues到目录: %s\n", len(issues), outputDir)

	// 如果启用了AI分析，生成总结
	if *withAI {
		// 优先使用命令行参数中的token
		tokenKey := *aiToken

		// 如果命令行没有提供，尝试从环境变量获取GitHub token
		if tokenKey == "" {
			tokenKey = os.Getenv("AI_TOKEN")
		}

		if tokenKey == "" {
			log.Println("警告: 启用了AI分析但未提供AI Token，跳过分析")
		} else {
			fmt.Println("正在使用AI分析issues...")
			if err := generateAISummary(issues, outputDir, *summaryFile, tokenKey); err != nil {
				log.Printf("AI分析失败: %v", err)
			} else {
				fmt.Printf("AI分析完成，总结已保存到: %s\n", filepath.Join(outputDir, *summaryFile))
			}
		}
	}
}

// 创建GitHub客户端
func createGitHubClient(tokenParam string) *github.Client {
	// 优先使用命令行参数中的token
	token := tokenParam

	// 如果命令行没有提供，尝试从环境变量获取GitHub token
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}

	if token == "" {
		// 如果没有token，使用匿名客户端（有API限制）
		fmt.Println("提示: 未提供GitHub Token，使用匿名访问（API限制较严格）")
		return github.NewClient(nil)
	}

	// 使用token创建认证客户端
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	tc := oauth2.NewClient(context.Background(), ts)
	return github.NewClient(tc)
}

// 获取仓库的所有issues
func fetchIssues(client *github.Client, owner, repo string) ([]*github.Issue, error) {
	ctx := context.Background()

	var allIssues []*github.Issue
	opts := &github.IssueListByRepoOptions{
		State: "all", // 获取所有状态的issues
		ListOptions: github.ListOptions{
			PerPage: 100, // 每页100个
		},
	}

	for {
		issues, resp, err := client.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("获取issues失败: %w", err)
		}

		allIssues = append(allIssues, issues...)

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return allIssues, nil
}

// 获取issue的所有评论
func fetchComments(client *github.Client, owner, repo string, issueNumber int) ([]*github.IssueComment, error) {
	ctx := context.Background()

	var allComments []*github.IssueComment
	opts := &github.IssueListCommentsOptions{
		ListOptions: github.ListOptions{
			PerPage: 100,
		},
	}

	for {
		comments, resp, err := client.Issues.ListComments(ctx, owner, repo, issueNumber, opts)
		if err != nil {
			return nil, fmt.Errorf("获取评论失败: %w", err)
		}

		allComments = append(allComments, comments...)

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return allComments, nil
}

// 将issue保存为Markdown文件
func saveIssueAsMarkdown(issue *github.Issue, outputDir, owner, repo string, client *github.Client, withComments bool) error {
	// 生成文件名，避免特殊字符
	title := sanitizeFilename(issue.GetTitle())
	filename := fmt.Sprintf("issue_%d_%s.md", issue.GetNumber(), title)
	path := filepath.Join(outputDir, filename)

	var comments []*github.IssueComment
	var err error

	// 根据参数决定是否获取评论
	if withComments {
		// 获取issue评论
		comments, err = fetchComments(client, owner, repo, issue.GetNumber())
		if err != nil {
			return fmt.Errorf("获取评论失败: %w", err)
		}
	}

	// 生成Markdown内容
	content := generateMarkdownContent(issue, comments)

	// 写入文件
	return os.WriteFile(path, []byte(content), 0644)
}

// 生成Markdown内容
func generateMarkdownContent(issue *github.Issue, comments []*github.IssueComment) string {
	var sb strings.Builder

	// 标题
	sb.WriteString(fmt.Sprintf("# Issue #%d: %s\n\n", issue.GetNumber(), issue.GetTitle()))

	// 基本信息
	sb.WriteString("## 基本信息\n\n")
	sb.WriteString(fmt.Sprintf("- **编号**: #%d\n", issue.GetNumber()))
	sb.WriteString(fmt.Sprintf("- **状态**: %s\n", issue.GetState()))
	sb.WriteString(fmt.Sprintf("- **创建者**: @%s\n", issue.GetUser().GetLogin()))
	sb.WriteString(fmt.Sprintf("- **创建时间**: %s\n", issue.GetCreatedAt().Format("2006-01-02 15:04:05")))

	if !issue.GetUpdatedAt().IsZero() {
		sb.WriteString(fmt.Sprintf("- **更新时间**: %s\n", issue.GetUpdatedAt().Format("2006-01-02 15:04:05")))
	}

	if issue.ClosedAt != nil {
		sb.WriteString(fmt.Sprintf("- **关闭时间**: %s\n", issue.GetClosedAt().Format("2006-01-02 15:04:05")))
	}

	// 标签
	if len(issue.Labels) > 0 {
		sb.WriteString("- **标签**: ")
		labels := make([]string, len(issue.Labels))
		for i, label := range issue.Labels {
			labels[i] = fmt.Sprintf("`%s`", label.GetName())
		}
		sb.WriteString(strings.Join(labels, ", "))
		sb.WriteString("\n")
	}

	// 指派人
	if len(issue.Assignees) > 0 {
		sb.WriteString("- **指派给**: ")
		assignees := make([]string, len(issue.Assignees))
		for i, assignee := range issue.Assignees {
			assignees[i] = fmt.Sprintf("@%s", assignee.GetLogin())
		}
		sb.WriteString(strings.Join(assignees, ", "))
		sb.WriteString("\n")
	}

	// 里程碑
	if issue.Milestone != nil {
		sb.WriteString(fmt.Sprintf("- **里程碑**: %s\n", issue.Milestone.GetTitle()))
	}

	sb.WriteString(fmt.Sprintf("- **链接**: %s\n\n", issue.GetHTMLURL()))

	// 描述内容
	if body := issue.GetBody(); body != "" {
		sb.WriteString("## 描述\n\n")
		sb.WriteString(body)
		sb.WriteString("\n\n")
	}

	// 评论部分
	if len(comments) > 0 {
		sb.WriteString("---\n\n")
		sb.WriteString("## 评论\n\n")

		for _, comment := range comments {
			sb.WriteString(fmt.Sprintf("### @%s 评论于 %s\n\n",
				comment.GetUser().GetLogin(),
				comment.GetCreatedAt().Format("2006-01-02 15:04:05")))
			sb.WriteString(comment.GetBody())
			sb.WriteString("\n\n---\n\n")
		}
	}

	return sb.String()
}

// 清理文件名中的特殊字符
func sanitizeFilename(filename string) string {
	// 替换或删除不适合文件名的字符
	replacer := strings.NewReplacer(
		"/", "_",
		"\\", "_",
		":", "_",
		"*", "_",
		"?", "_",
		"\"", "_",
		"<", "_",
		">", "_",
		"|", "_",
		"\n", "_",
		"\r", "_",
	)

	cleaned := replacer.Replace(filename)

	// 限制长度
	if len(cleaned) > 50 {
		cleaned = cleaned[:50]
	}

	return strings.TrimSpace(cleaned)
}
