package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-github/v57/github"
	log "github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
)

const (
	EnvGitHubToken = "GITHUB_TOKEN"
	EnvAIToken     = "AI_TOKEN"
)

// normalizeArgs 把位置参数（仓库地址）挪到所有 flag 之后。
// Go 标准库的 flag 包在遇到第一个非 flag 参数时会停止解析，
// 这会导致 `issue2file owner/repo -ai` 中的 -ai 被当成位置参数而静默失效；
// README 里的用法正是这种顺序，所以这里先做一次重排再交给 flag 解析。
func normalizeArgs(args []string) []string {
	var flags, positional []string

	isFlagToken := func(s string) bool {
		return len(s) > 1 && strings.HasPrefix(s, "-") && s != "--"
	}

	for i := 0; i < len(args); i++ {
		a := args[i]
		if !isFlagToken(a) {
			positional = append(positional, a)
			continue
		}

		flags = append(flags, a)

		// -name=value 形式自带取值，不需要再消费后续参数
		if strings.Contains(a, "=") {
			continue
		}

		name := strings.TrimLeft(a, "-")
		f := flag.Lookup(name)
		if f == nil {
			continue // 未知 flag，交给 flag 包去报错
		}
		// 只有非 bool 类型的 flag 才会以独立参数的形式跟一个取值
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) && !isFlagToken(args[i+1]) {
			i++
			flags = append(flags, args[i])
		}
	}

	return append(flags, positional...)
}

func main() {
	// 定义命令行参数
	var (
		token     = flag.String("token", "", "GitHub API token")
		aiToken   = flag.String("aiToken", "", "AI API token")
		aiModel   = flag.String("aiModel", "deepseek-chat", "AI model name")
		aiBaseURL = flag.String("aiBaseURL", "https://api.deepseek.com/v1/chat/completions", "AI base URL")

		commentEnable = flag.Bool("comment", false, "是否下载issue评论")
		aiEnable      = flag.Bool("ai", false, "是否使用AI分析issues")
		chartEnable   = flag.Bool("chart", false, "是否生成图表分析")
		// 可视化面板默认开启：这是本工具最主要的产出之一
		dashboardEnable = flag.Bool("dashboard", true, "是否生成可视化面板 dashboard.html 与 issues.json")

		includePR = flag.Bool("includePR", false, "是否把 Pull Request 也当作 issue 导出（默认过滤）")
		serve     = flag.Bool("serve", false, "导出完成后在本地启动 Web 服务浏览面板")
		port      = flag.Int("port", 8080, "-serve 模式下的监听端口")

		outputDir   = flag.String("output", "", "指定输出目录")
		summaryFile = flag.String("filename", "summary.md", "AI分析总结文件名")
		configFile  = flag.String("config", "config.example.conf", "指定配置文件路径，配置文件中的参数会覆盖命令行参数")
	)

	// 解析命令行参数
	flag.CommandLine.Parse(normalizeArgs(os.Args[1:]))

	// 如果指定了配置文件，则加载配置文件
	var config *Config
	if *configFile != "" {
		var err error
		config, err = LoadConfig(*configFile)
		if err != nil {
			log.Fatalf("加载配置文件失败: %v \n", err)
		}
		fmt.Printf("已加载配置文件: %s\n", *configFile)

		// 使用配置文件中的参数覆盖命令行参数
		if config.GitHubToken != "" {
			*token = config.GitHubToken
		}
		if config.AIToken != "" {
			*aiToken = config.AIToken
		}
		if config.AIModel != "" {
			*aiModel = config.AIModel
		}
		if config.AIBaseURL != "" {
			*aiBaseURL = config.AIBaseURL
		}
		// 只有当配置文件中明确声明了这些键时才覆盖命令行参数
		if config.CommentEnable != nil {
			*commentEnable = *config.CommentEnable
		}
		if config.AiEnable != nil {
			*aiEnable = *config.AiEnable
		}
		if config.ChartEnable != nil {
			*chartEnable = *config.ChartEnable
		}
		if config.DashboardEnable != nil {
			*dashboardEnable = *config.DashboardEnable
		}
		if config.IncludePR != nil {
			*includePR = *config.IncludePR
		}
		if config.Serve != nil {
			*serve = *config.Serve
		}
		if config.Port > 0 {
			*port = config.Port
		}
		if config.OutputDir != "" {
			*outputDir = config.OutputDir
		}
		if config.SummaryFile != "" {
			*summaryFile = config.SummaryFile
		}
		// 打印脱敏后的配置摘要，不要把 token 打到终端或日志
		fmt.Printf("config: %s\n", config)
	}

	// 检查是否提供了仓库参数
	args := flag.Args()
	if len(args) < 1 {
		fmt.Println("使用方法: issue2file [选项] <仓库地址>")
		fmt.Println("选项:")
		flag.PrintDefaults()
		fmt.Println("\n示例:")
		fmt.Println("  issue2file .                    # 从当前目录的git仓库获取issues")
		fmt.Println("  issue2file owner/repo           # 导出并生成可视化面板 dashboard.html")
		fmt.Println("  issue2file owner/repo -serve    # 导出后启动本地服务，浏览器打开面板")
		fmt.Println("  issue2file owner/repo -ai       # 使用AI分析issues")
		fmt.Println("  issue2file -config=config.cnf owner/repo # 使用配置文件")
		fmt.Println()
		fmt.Println("提示: GitHub Token 建议用环境变量 GITHUB_TOKEN 传入，避免出现在命令行历史中")
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
	issues, err := fetchIssues(client, owner, repo, *includePR)
	if err != nil {
		log.Fatalf("获取issues失败: %v", err)
	}
	if len(issues) == 0 {
		log.Fatalf("仓库 %s/%s 没有可导出的 issue（也可能是 API 限流或权限不足）", owner, repo)
	}

	// 创建输出目录
	var output string
	if *outputDir != "" {
		output = *outputDir
	} else {
		output = fmt.Sprintf("issues_%s_%s", owner, repo)
	}
	if err := os.MkdirAll(output, 0755); err != nil {
		log.Fatalf("创建输出目录失败: %v", err)
	}

	// 保存issues为Markdown文件
	for _, issue := range issues {
		if err := saveIssueAsMarkdown(issue, output, owner, repo, client, *commentEnable); err != nil {
			log.Printf("保存issue #%d 失败: %v", issue.GetNumber(), err)
		} else {
			fmt.Printf("已保存 issue #%d: %s\n", issue.GetNumber(), issue.GetTitle())
		}
	}

	fmt.Printf("完成！共保存了 %d 个issues到目录: %s\n", len(issues), output)

	// 生成可视化面板（dashboard.html + issues.json）
	if *dashboardEnable {
		fmt.Println("正在生成可视化面板...")
		page, err := GenerateDashboard(owner, repo, output, issues)
		if err != nil {
			log.Printf("可视化面板生成失败: %v", err)
		} else {
			fmt.Printf("可视化面板已生成: %s（数据集: %s）\n", page, filepath.Join(output, "issues.json"))
		}
	}

	// 如果启用了AI分析，生成总结
	if *aiEnable {
		// 优先使用命令行参数中的token
		tokenKey := *aiToken

		// 如果命令行没有提供，尝试从环境变量获取GitHub token
		if tokenKey == "" {
			tokenKey = os.Getenv(EnvAIToken)
		}

		if tokenKey == "" {
			log.Println("警告: 启用了AI分析但未提供AI Token，跳过分析")
		} else {
			fmt.Println("正在使用AI分析issues...")
			if err := generateAISummary(issues, output, *summaryFile, tokenKey, *aiModel, *aiBaseURL); err != nil {
				log.Printf("AI分析失败: %v", err)
			} else {
				fmt.Printf("AI分析完成，总结已保存到: %s\n", filepath.Join(output, *summaryFile))
			}
		}
	}

	// 如果启用了图表生成，生成图表
	if *chartEnable {
		fmt.Println("正在生成图表...")
		if err := generateCharts(issues, output); err != nil {
			log.Printf("图表生成失败: %v", err)
		} else {
			fmt.Printf("图表生成完成，可在 %s/charts 目录查看\n", output)
		}
	}

	// 启动本地 Web 服务浏览产出（阻塞直到 Ctrl+C）
	if *serve {
		if err := serveOutput(output, *port); err != nil {
			log.Fatalf("启动本地服务失败: %v", err)
		}
	}
}

// 创建GitHub客户端
func createGitHubClient(tokenParam string) *github.Client {
	// 优先使用命令行参数中的token
	token := tokenParam

	// 如果命令行没有提供，尝试从环境变量获取GitHub token
	if token == "" {
		token = os.Getenv(EnvGitHubToken)
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
// GitHub 的 /issues 接口会把 Pull Request 一并返回（每个 PR 在 REST 语义上也是一个 issue），
// 默认按 IsPullRequest() 过滤掉，否则统计与图表口径会被 PR 污染。
func fetchIssues(client *github.Client, owner, repo string, includePR bool) ([]*github.Issue, error) {
	ctx := context.Background()

	var allIssues []*github.Issue
	var pulled int
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

		pulled += len(issues)
		for _, issue := range issues {
			if !includePR && issue.IsPullRequest() {
				continue
			}
			allIssues = append(allIssues, issue)
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	if !includePR && pulled != len(allIssues) {
		fmt.Printf("已过滤 %d 个 Pull Request，剩余 %d 个 issue\n", pulled-len(allIssues), len(allIssues))
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

	cleaned := strings.TrimSpace(replacer.Replace(filename))

	// 按 rune 截断：按字节截断会把多字节字符切一半，产出非法 UTF-8 的文件名
	runes := []rune(cleaned)
	if len(runes) > 50 {
		runes = runes[:50]
	}
	// 结尾的点或空格在 Windows 上不允许
	cleaned = strings.TrimRight(string(runes), ". ")

	if cleaned == "" {
		return "untitled"
	}
	return cleaned
}
