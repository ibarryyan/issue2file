package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-github/v57/github"
	log "github.com/sirupsen/logrus"
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
		aiBaseURL = flag.String("aiBaseURL", "https://api.deepseek.com/v1", "AI base URL（前缀即可，SDK 会自动补 /chat/completions）")

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

		web       = flag.Bool("web", false, "启动 Web 交互界面（不带仓库参数时默认进入）")
		noBrowser = flag.Bool("noBrowser", false, "Web 模式下不自动打开浏览器")
	)

	// 解析命令行参数
	flag.CommandLine.Parse(normalizeArgs(os.Args[1:]))

	// 记录用户在命令行上显式写出的 flag —— 这些值不应被配置文件再覆盖一次。
	// 之前只对 config 做了这个判断，结果 `-web -port 9000` 这类显式参数
	// 会被默认配置文件 config.example.conf 里的 port 悄悄改掉，
	// 用户在终端看到的端口和自己在命令行写的不一致，且没有任何提示。
	setFlags := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })
	configExplicit := setFlags["config"]

	// 如果指定了配置文件，则加载配置文件
	var config *Config
	if *configFile != "" {
		var err error
		config, err = LoadConfig(*configFile)
		if err != nil {
			if configExplicit {
				log.Fatalf("加载配置文件失败: %v \n", err)
			}
			if errors.Is(err, os.ErrNotExist) {
				// 默认示例配置没拷过来属于预期情况，静默跳过即可
				log.Debugf("未加载默认配置文件 %s: %v", *configFile, err)
			} else {
				// 文件存在但内容有问题（TOML 语法错误、权限不足等）必须让用户看见，
				// 否则写错一行会让 token、输出目录等配置全部静默失效。
				log.Warnf("配置文件 %s 存在但加载失败，本次运行已忽略该文件：%v", *configFile, err)
			}
			config = nil
		}
	}

	if config != nil {
		fmt.Printf("已加载配置文件: %s\n", *configFile)

		// 使用配置文件补充未在命令行显式指定的参数。
		// 命令行优先：显式写出过的 flag 一律保持原值。
		if !setFlags["token"] && config.GitHubToken != "" {
			*token = config.GitHubToken
		}
		if !setFlags["aiToken"] && config.AIToken != "" {
			*aiToken = config.AIToken
		}
		if !setFlags["aiModel"] && config.AIModel != "" {
			*aiModel = config.AIModel
		}
		if !setFlags["aiBaseURL"] && config.AIBaseURL != "" {
			*aiBaseURL = config.AIBaseURL
		}
		// 只有当配置文件中明确声明了这些键时才覆盖命令行参数
		if !setFlags["comment"] && config.CommentEnable != nil {
			*commentEnable = *config.CommentEnable
		}
		if !setFlags["ai"] && config.AiEnable != nil {
			*aiEnable = *config.AiEnable
		}
		if !setFlags["chart"] && config.ChartEnable != nil {
			*chartEnable = *config.ChartEnable
		}
		if !setFlags["dashboard"] && config.DashboardEnable != nil {
			*dashboardEnable = *config.DashboardEnable
		}
		if !setFlags["includePR"] && config.IncludePR != nil {
			*includePR = *config.IncludePR
		}
		if !setFlags["serve"] && config.Serve != nil {
			*serve = *config.Serve
		}
		if !setFlags["port"] && config.Port > 0 {
			*port = config.Port
		}
		if !setFlags["output"] && config.OutputDir != "" {
			*outputDir = config.OutputDir
		}
		if !setFlags["filename"] && config.SummaryFile != "" {
			*summaryFile = config.SummaryFile
		}
		// 打印脱敏后的配置摘要，不要把 token 打到终端或日志
		fmt.Printf("config: %s\n", config)
	}

	// 检查是否提供了仓库参数
	args := flag.Args()

	// 不带仓库参数（或显式 -web）时进入 Web 交互模式：
	// 页面里填仓库链接、必要时填 Token，拉取 issue 后可多选做深度分析。
	if *web || len(args) < 1 {
		if len(args) < 1 {
			fmt.Println("未指定仓库，进入 Web 交互模式（在页面填写仓库链接即可开始）")
		}
		ghToken := *token
		if ghToken == "" {
			ghToken = os.Getenv(EnvGitHubToken)
		}
		aiKey := *aiToken
		if aiKey == "" {
			aiKey = os.Getenv(EnvAIToken)
		}
		if err := runWebServer(webOptions{
			Port:        *port,
			GitHubToken: ghToken,
			AIToken:     aiKey,
			AIModel:     *aiModel,
			AIBaseURL:   *aiBaseURL,
			OpenBrowser: !*noBrowser,
		}); err != nil {
			log.Fatalf("Web 服务异常退出: %v", err)
		}
		return
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
	}

	client, err := newGitHubClient(token, "")
	if err != nil {
		// newGitHubClient 只在 baseURL 非法时才出错，这里传的是空串，不会发生
		log.Fatalf("创建GitHub客户端失败: %v", err)
	}
	return client
}

// 获取仓库的所有issues
// GitHub 的 /issues 接口会把 Pull Request 一并返回（每个 PR 在 REST 语义上也是一个 issue），
// 默认按 IsPullRequest() 过滤掉，否则统计与图表口径会被 PR 污染。
func fetchIssues(client *github.Client, owner, repo string, includePR bool) ([]*github.Issue, error) {
	return fetchIssuesWithContext(context.Background(), client, owner, repo, includePR, 0)
}

// maxIssuePages 是分页硬上限：即使 API 一直返回 NextPage 也不会无限循环。
const maxIssuePages = 500

// fetchIssuesWithContext 支持调用方传入 ctx（Web 模式需要超时控制）。
// limit > 0 时最多返回 limit 条，用于给超大仓库兜底。
func fetchIssuesWithContext(ctx context.Context, client *github.Client, owner, repo string, includePR bool, limit int) ([]*github.Issue, error) {
	var allIssues []*github.Issue
	var prFiltered int
	var truncated bool
	opts := &github.IssueListByRepoOptions{
		State: "all", // 获取所有状态的issues
		ListOptions: github.ListOptions{
			PerPage: 100, // 每页100个
		},
	}

	for page := 0; page < maxIssuePages; page++ {
		issues, resp, err := client.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			// 用 %w 保留原始错误：调用方（Web 模式）需要 errors.As 识别出
			// 403/404/限流才能给出可照做的提示。
			return nil, fmt.Errorf("获取issues失败: %w", err)
		}

		for _, issue := range issues {
			if !includePR && issue.IsPullRequest() {
				prFiltered++
				continue
			}
			if limit > 0 && len(allIssues) >= limit {
				truncated = true
				break
			}
			allIssues = append(allIssues, issue)
		}

		if resp.NextPage == 0 || truncated {
			break
		}
		opts.Page = resp.NextPage
	}

	// 分别计数，避免截断时把"被截断的 issue"算进"被过滤的 PR"里
	if prFiltered > 0 {
		fmt.Printf("已过滤 %d 个 Pull Request，剩余 %d 个 issue\n", prFiltered, len(allIssues))
	}
	if truncated {
		fmt.Printf("issue 数量超过上限 %d，已截断\n", limit)
	}

	return allIssues, nil
}

// 获取issue的所有评论
func fetchComments(client *github.Client, owner, repo string, issueNumber int) ([]*github.IssueComment, error) {
	return fetchCommentsWithContext(context.Background(), client, owner, repo, issueNumber)
}

func fetchCommentsWithContext(ctx context.Context, client *github.Client, owner, repo string, issueNumber int) ([]*github.IssueComment, error) {
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
