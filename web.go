package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/go-github/v57/github"
	log "github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
)

//go:embed templates/index.html
var indexFS embed.FS

const (
	// sessionTTL 会话空闲多久后失效。issue 数据留在内存里，进程退出即消失。
	sessionTTL = 30 * time.Minute
	// maxSessions 内存里最多保留多少个仓库会话，超出后淘汰最旧的。
	maxSessions = 32
	// maxSelected 一次深度分析最多处理多少个 issue，避免请求体过大。
	maxSelected = 300
	// maxCommentIssues 深度分析时最多拉取多少个 issue 的评论，保护 API 配额。
	maxCommentIssues = 50
	// maxIssuesPerRepo 单个仓库最多拉取多少个 issue。
	maxIssuesPerRepo = 5000
	// maxBodySize API 请求体上限。
	maxBodySize = 1 << 20
	// githubTimeout 单次 GitHub 请求的整体超时。
	githubTimeout = 90 * time.Second
	// aiTimeout AI 分析的超时。
	aiTimeout = 3 * time.Minute
)

// repoInfo 是仓库的基础信息，展示在页面顶部。
type repoInfo struct {
	FullName      string   `json:"fullName"`
	Description   string   `json:"description"`
	Stars         int      `json:"stars"`
	Forks         int      `json:"forks"`
	Watchers      int      `json:"watchers"`
	OpenIssues    int      `json:"openIssues"`
	Language      string   `json:"language"`
	License       string   `json:"license"`
	DefaultBranch string   `json:"defaultBranch"`
	Topics        []string `json:"topics"`
	Private       bool     `json:"private"`
	Fork          bool     `json:"fork"`
	Archived      bool     `json:"archived"`
	CreatedAt     string   `json:"createdAt"`
	UpdatedAt     string   `json:"updatedAt"`
	URL           string   `json:"url"`
}

// session 保存一次仓库拉取的结果，供后续「选中分析」复用，避免重复请求 GitHub。
type session struct {
	ID         string
	Owner      string
	Repo       string
	Token      string
	Issues     []*github.Issue
	Created    time.Time
	LastAccess time.Time
}

type sessionStore struct {
	mu    sync.Mutex
	ttl   time.Duration
	max   int
	items map[string]*session
	order []string
}

func newSessionStore(ttl time.Duration, max int) *sessionStore {
	return &sessionStore{ttl: ttl, max: max, items: make(map[string]*session)}
}

// Put 存入一次拉取结果并返回 session。
func (s *sessionStore) Put(owner, repo, token string, issues []*github.Issue) *session {
	id, err := randomID()
	if err != nil {
		// 随机数来源不可用时退化为时间戳，宁可 ID 可预测也不能拉取失败
		id = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	sess := &session{
		ID:         id,
		Owner:      owner,
		Repo:       repo,
		Token:      token,
		Issues:     issues,
		Created:    time.Now(),
		LastAccess: time.Now(),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == nil {
		s.items = make(map[string]*session)
	}
	s.items[id] = sess
	s.order = append(s.order, id)
	s.sweepLocked(time.Now())
	return sess
}

// Get 取出会话并刷新访问时间；不存在或已过期返回 nil。
func (s *sessionStore) Get(id string) *session {
	if id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.items[id]
	if !ok {
		return nil
	}
	now := time.Now()
	if s.ttl > 0 && now.Sub(sess.LastAccess) > s.ttl {
		delete(s.items, id)
		s.removeFromOrder(id)
		return nil
	}
	sess.LastAccess = now
	return sess
}

func (s *sessionStore) sweepLocked(now time.Time) {
	// 先按 TTL 清理。必须遍历副本：removeFromOrder 会就地缩容 s.order，
	// 直接 range 原切片会读到被搬移过的元素。
	if s.ttl > 0 {
		for _, id := range append([]string(nil), s.order...) {
			sess, ok := s.items[id]
			if !ok {
				continue
			}
			if now.Sub(sess.LastAccess) > s.ttl {
				delete(s.items, id)
				s.removeFromOrder(id)
			}
		}
	}
	// 再按容量淘汰最旧的
	for len(s.order) > s.max && len(s.order) > 0 {
		id := s.order[0]
		s.order = s.order[1:]
		delete(s.items, id)
	}
}

func (s *sessionStore) removeFromOrder(id string) {
	for i, v := range s.order {
		if v == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			return
		}
	}
}

func randomID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// webOptions 是 Web 模式的启动参数。
type webOptions struct {
	Port         int
	GitHubToken  string // 来自 -token / 配置文件 / GITHUB_TOKEN，请求未带 token 时使用
	AIToken      string // 来自 -aiToken / AI_TOKEN
	AIModel      string
	AIBaseURL    string
	OpenBrowser  bool
	APIBaseURL   string // 仅测试用：指向 httptest 服务器
	SessionStore *sessionStore
}

type webServer struct {
	opts  webOptions
	index []byte
	store *sessionStore
}

func newWebServer(o webOptions) (*webServer, error) {
	raw, err := indexFS.ReadFile("templates/index.html")
	if err != nil {
		return nil, fmt.Errorf("读取内置页面失败: %w", err)
	}
	store := o.SessionStore
	if store == nil {
		store = newSessionStore(sessionTTL, maxSessions)
	}
	return &webServer{opts: o, index: raw, store: store}, nil
}

func (s *webServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/fetch", s.handleFetch)
	mux.HandleFunc("/api/analyze", s.handleAnalyze)
	mux.HandleFunc("/api/export", s.handleExport)
	return mux
}

// ---------- 请求 / 响应结构 ----------

type fetchRequest struct {
	Repo      string `json:"repo"`
	Token     string `json:"token"`
	AIToken   string `json:"aiToken"`
	IncludePR bool   `json:"includePR"`
	UseAI     bool   `json:"useAI"`
}

type fetchResponse struct {
	SessionID string        `json:"sessionId"`
	Repo      repoInfo      `json:"repo"`
	Data      DashboardData `json:"data"`
	Report    string        `json:"report"`
	AIInsight string        `json:"aiInsight"`
	Warnings  []string      `json:"warnings"`
}

type analyzeRequest struct {
	SessionID    string `json:"sessionId"`
	Numbers      []int  `json:"numbers"`
	WithComments bool   `json:"withComments"`
	AIToken      string `json:"aiToken"`
	UseAI        bool   `json:"useAI"`
}

type analyzeResponse struct {
	SessionID    string          `json:"sessionId"`
	Count        int             `json:"count"`
	Items        []issueAnalysis `json:"items"`
	Report       string          `json:"report"`
	CategoryDist []NameValue     `json:"categoryDist"`
	PriorityDist []NameValue     `json:"priorityDist"`
	KeywordDist  []NameValue     `json:"keywordDist"`
	AgeDist      []NameValue     `json:"ageDist"`
	AIInsight    string          `json:"aiInsight"`
	AIUsed       bool            `json:"aiUsed"`
	Warnings     []string        `json:"warnings"`
}

type exportRequest struct {
	SessionID    string `json:"sessionId"`
	Numbers      []int  `json:"numbers"`
	WithComments bool   `json:"withComments"`
	Report       string `json:"report"`
}

type exportResponse struct {
	Filename string `json:"filename"`
	Content  string `json:"content"`
}

// ---------- handlers ----------

func (s *webServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 GET")
		return
	}
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(s.index)
}

func (s *webServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *webServer) handleConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"hasGitHubToken": s.opts.GitHubToken != "",
		"hasAIToken":     s.opts.AIToken != "",
		"aiModel":        s.opts.AIModel,
	})
}

func (s *webServer) handleFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var req fetchRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	repoArg := strings.TrimSpace(req.Repo)
	if repoArg == "" {
		writeErr(w, http.StatusBadRequest, "请填写仓库地址，例如 https://github.com/owner/repo 或 owner/repo")
		return
	}
	owner, repo, err := parseRepoURL(repoArg)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "无法解析仓库地址："+repoArg+
			"（支持 https://github.com/owner/repo、git@github.com:owner/repo.git、owner/repo）")
		return
	}

	token := strings.TrimSpace(req.Token)
	if token == "" {
		token = s.opts.GitHubToken
	}
	client, err := newGitHubClient(token, s.opts.APIBaseURL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), githubTimeout)
	defer cancel()

	info, warnRepo, err := fetchRepoInfo(ctx, client, owner, repo)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	issues, err := fetchIssuesWithContext(ctx, client, owner, repo, req.IncludePR, maxIssuesPerRepo)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}

	var warnings []string
	if warnRepo != "" {
		warnings = append(warnings, warnRepo)
	}
	if token == "" {
		warnings = append(warnings, "当前使用匿名访问（API 限额 60 次/小时），超大仓库或多私有仓库建议填写 Token。")
	}
	if len(issues) >= maxIssuesPerRepo {
		warnings = append(warnings, fmt.Sprintf("issue 数量达到上限 %d，结果已截断。", maxIssuesPerRepo))
	}
	if len(issues) == 0 {
		warnings = append(warnings, "该仓库没有可读取的 issue，可能尚未开启 issues 或全部是 Pull Request。")
	}

	data := BuildDashboardDataWith(owner, repo, issues, req.IncludePR)
	report := BuildOverviewReport(data, info)

	resp := fetchResponse{
		Repo:     info,
		Data:     data,
		Report:   report,
		Warnings: warnings,
	}

	// 只有拉到了 issue 才建会话，否则「选中分析」没有意义
	if len(issues) > 0 {
		resp.SessionID = s.store.Put(owner, repo, token, issues).ID
	}

	if req.UseAI {
		aiToken := strings.TrimSpace(req.AIToken)
		if aiToken == "" {
			aiToken = s.opts.AIToken
		}
		switch {
		case aiToken == "":
			warnings = append(warnings, "未配置 AI Token，已生成本地统计报告（不含 AI 洞察）。")
		case len(issues) == 0:
			warnings = append(warnings, "没有 issue，跳过 AI 分析。")
		default:
			insight, err := aiOverview(issues, data, aiToken, s.opts.AIModel, s.opts.AIBaseURL)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("AI 分析失败，已回退到本地统计：%v", err))
			} else {
				resp.AIInsight = insight
			}
		}
	}
	resp.Warnings = warnings

	writeJSON(w, http.StatusOK, resp)
}

func (s *webServer) handleAnalyze(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var req analyzeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	sess := s.store.Get(req.SessionID)
	if sess == nil {
		writeErr(w, http.StatusBadRequest, "会话已过期或不存在，请重新拉取一次 issue")
		return
	}
	if len(req.Numbers) == 0 {
		writeErr(w, http.StatusBadRequest, "请至少选择一个 issue 再做深度分析")
		return
	}

	selected := pickIssues(sess.Issues, req.Numbers)
	if len(selected) == 0 {
		writeErr(w, http.StatusBadRequest, "所选 issue 不在当前会话中，请重新拉取")
		return
	}

	var warnings []string
	if len(selected) > maxSelected {
		warnings = append(warnings, fmt.Sprintf("选中数量超过 %d，仅分析前 %d 个。", maxSelected, maxSelected))
		selected = selected[:maxSelected]
	}

	comments := make(map[int][]*github.IssueComment)
	if req.WithComments {
		if len(selected) > maxCommentIssues {
			warnings = append(warnings, fmt.Sprintf("选中数量超过 %d，已跳过评论拉取以节省 API 配额。", maxCommentIssues))
		} else {
			client, err := newGitHubClient(sess.Token, s.opts.APIBaseURL)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), githubTimeout)
			defer cancel()
			for _, it := range selected {
				// 评论数为 0 的 issue 不必发请求，省下的是真金白银的 API 配额
				if it.GetComments() == 0 {
					continue
				}
				cs, err := fetchCommentsWithContext(ctx, client, sess.Owner, sess.Repo, it.GetNumber())
				if err != nil {
					warnings = append(warnings, fmt.Sprintf("拉取 #%d 评论失败：%v", it.GetNumber(), err))
					continue
				}
				comments[it.GetNumber()] = cs
			}
		}
	}

	items, catDist, prioDist, kwDist, ageDist := deepAnalyze(selected, time.Now(), comments)

	var aiInsight string
	aiUsed := false
	if req.UseAI {
		aiToken := strings.TrimSpace(req.AIToken)
		if aiToken == "" {
			aiToken = s.opts.AIToken
		}
		if aiToken == "" {
			warnings = append(warnings, "未配置 AI Token，深度分析仅使用本地规则引擎。")
		} else {
			out, err := aiDeepInsight(selected, items, aiToken, s.opts.AIModel, s.opts.AIBaseURL)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("AI 分析失败，已回退到本地规则：%v", err))
			} else {
				aiInsight = out
				aiUsed = true
			}
		}
	}

	meta := Meta{
		Owner:       sess.Owner,
		Repo:        sess.Repo,
		FullName:    sess.Owner + "/" + sess.Repo,
		RepoURL:     fmt.Sprintf("https://github.com/%s/%s", sess.Owner, sess.Repo),
		GeneratedAt: time.Now().Format(layoutDateTime),
		Source:      "github-rest-api-v3",
	}
	report := BuildDeepReport(items, meta, catDist, prioDist, kwDist, ageDist, aiInsight)

	writeJSON(w, http.StatusOK, analyzeResponse{
		SessionID:    sess.ID,
		Count:        len(items),
		Items:        items,
		Report:       report,
		CategoryDist: catDist,
		PriorityDist: prioDist,
		KeywordDist:  kwDist,
		AgeDist:      ageDist,
		AIInsight:    aiInsight,
		AIUsed:       aiUsed,
		Warnings:     warnings,
	})
}

func (s *webServer) handleExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var req exportRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	sess := s.store.Get(req.SessionID)
	if sess == nil {
		writeErr(w, http.StatusBadRequest, "会话已过期或不存在，请重新拉取一次 issue")
		return
	}

	// numbers 为空表示导出全部
	var list []*github.Issue
	if len(req.Numbers) == 0 {
		list = sess.Issues
	} else {
		list = pickIssues(sess.Issues, req.Numbers)
	}
	if len(list) == 0 {
		writeErr(w, http.StatusBadRequest, "没有可导出的 issue")
		return
	}

	comments := make(map[int][]*github.IssueComment)
	var warnings []string
	if req.WithComments {
		if len(list) > maxCommentIssues {
			warnings = append(warnings, fmt.Sprintf("数量超过 %d，已跳过评论导出。", maxCommentIssues))
		} else {
			client, err := newGitHubClient(sess.Token, s.opts.APIBaseURL)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), githubTimeout)
			defer cancel()
			for _, it := range list {
				cs, err := fetchCommentsWithContext(ctx, client, sess.Owner, sess.Repo, it.GetNumber())
				if err != nil {
					warnings = append(warnings, fmt.Sprintf("拉取 #%d 评论失败：%v", it.GetNumber(), err))
					continue
				}
				comments[it.GetNumber()] = cs
			}
		}
	}

	content := buildExportMarkdown(sess.Owner, sess.Repo, list, comments, req.Report, warnings)
	name := fmt.Sprintf("issues_%s_%s_%s.md", sess.Owner, sess.Repo, time.Now().Format("20060102"))
	writeJSON(w, http.StatusOK, exportResponse{Filename: safeFileName(name), Content: content})
}

// buildExportMarkdown 把报告与 issue 正文拼成一份可下载的 Markdown。
func buildExportMarkdown(owner, repo string, issues []*github.Issue, comments map[int][]*github.IssueComment, report string, warnings []string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("# %s/%s Issues 导出\n\n", owner, repo))
	b.WriteString(fmt.Sprintf("- 导出时间：%s\n", time.Now().Format(layoutDateTime)))
	b.WriteString(fmt.Sprintf("- issue 数量：%d\n", len(issues)))
	b.WriteString(fmt.Sprintf("- 仓库地址：https://github.com/%s/%s\n", owner, repo))
	if len(warnings) > 0 {
		b.WriteString("\n> 提示：\n")
		for _, w := range warnings {
			b.WriteString(fmt.Sprintf("> - %s\n", w))
		}
	}
	if strings.TrimSpace(report) != "" {
		b.WriteString("\n---\n\n")
		b.WriteString(strings.TrimSpace(report))
		b.WriteString("\n")
	}
	for _, issue := range issues {
		b.WriteString("\n---\n\n")
		b.WriteString(generateMarkdownContent(issue, comments[issue.GetNumber()]))
	}
	return b.String()
}

// safeFileName 去掉路径分隔符，防止下载时被写成奇怪的路径。
func safeFileName(name string) string {
	return strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(name)
}

// pickIssues 按 numbers 的顺序挑出 issue，并去重。
func pickIssues(all []*github.Issue, numbers []int) []*github.Issue {
	byNum := make(map[int]*github.Issue, len(all))
	for _, it := range all {
		if it == nil {
			continue
		}
		byNum[it.GetNumber()] = it
	}
	out := make([]*github.Issue, 0, len(numbers))
	seen := make(map[int]bool, len(numbers))
	for _, n := range numbers {
		if seen[n] {
			continue
		}
		seen[n] = true
		if it, ok := byNum[n]; ok {
			out = append(out, it)
		}
	}
	return out
}

// ---------- GitHub 访问 ----------

// newGitHubClient 创建 GitHub 客户端。baseURL 非空时指向替代地址（测试用）。
func newGitHubClient(token, baseURL string) (*github.Client, error) {
	var client *github.Client
	if token == "" {
		client = github.NewClient(nil)
	} else {
		ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
		client = github.NewClient(oauth2.NewClient(context.Background(), ts))
	}
	if baseURL != "" {
		u, err := url.Parse(baseURL)
		if err != nil {
			return nil, fmt.Errorf("GitHub API 地址不合法: %w", err)
		}
		if !strings.HasSuffix(u.Path, "/") {
			u.Path += "/"
		}
		client.BaseURL = u
	}
	return client, nil
}

// fetchRepoInfo 获取仓库基础信息，并把 API 错误翻译成面向用户的提示。
func fetchRepoInfo(ctx context.Context, client *github.Client, owner, repo string) (repoInfo, string, error) {
	r, resp, err := client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return repoInfo{}, "", friendlyGitHubError(err, owner+"/"+repo)
	}
	var warn string
	if resp != nil && resp.Rate.Remaining == 0 {
		warn = "GitHub API 配额已用尽，请稍后重试或填写 Token 提升配额。"
	}

	info := repoInfo{
		FullName:      r.GetFullName(),
		Description:   r.GetDescription(),
		Stars:         r.GetStargazersCount(),
		Forks:         r.GetForksCount(),
		Watchers:      r.GetSubscribersCount(),
		OpenIssues:    r.GetOpenIssuesCount(),
		Language:      r.GetLanguage(),
		License:       r.GetLicense().GetSPDXID(),
		DefaultBranch: r.GetDefaultBranch(),
		Topics:        r.Topics,
		Private:       r.GetPrivate(),
		Fork:          r.GetFork(),
		Archived:      r.GetArchived(),
		CreatedAt:     r.GetCreatedAt().Format(layoutDate),
		UpdatedAt:     r.GetUpdatedAt().Format(layoutDate),
		URL:           r.GetHTMLURL(),
	}
	if info.License == "NOASSERTION" || info.License == "" {
		info.License = r.GetLicense().GetName()
	}
	if info.Topics == nil {
		info.Topics = []string{}
	}
	return info, warn, nil
}

// friendlyGitHubError 把常见的 GitHub 错误翻成能直接照做的提示。
func friendlyGitHubError(err error, fullName string) error {
	var ge *github.ErrorResponse
	if errors.As(err, &ge) {
		switch {
		case ge.Response != nil && ge.Response.StatusCode == http.StatusUnauthorized:
			return fmt.Errorf("Token 无效或已过期（401），请检查后重试")
		case ge.Response != nil && ge.Response.StatusCode == http.StatusForbidden:
			return fmt.Errorf("访问被拒绝（403）：可能触发了 API 限流或 Token 权限不足，建议填写 Token 后重试")
		case ge.Response != nil && ge.Response.StatusCode == http.StatusNotFound:
			return fmt.Errorf("仓库 %s 不存在或没有访问权限（404）。若为私有仓库，请在页面填写有 repo 权限的 Token", fullName)
		}
		if ge.Message != "" {
			return fmt.Errorf("GitHub 返回错误：%s", ge.Message)
		}
	}
	var rl *github.RateLimitError
	if errors.As(err, &rl) {
		return fmt.Errorf("GitHub API 限流（配额 %d 已用尽，%s 后重置），建议填写 Token 后重试",
			rl.Rate.Limit, rl.Rate.Reset.Time.Format("15:04:05"))
	}
	return fmt.Errorf("获取仓库信息失败：%v", err)
}

// ---------- AI ----------

func aiOverview(issues []*github.Issue, data DashboardData, token, model, baseURL string) (string, error) {
	if len(issues) == 0 {
		return "", errors.New("没有 issue 可供分析")
	}
	var b strings.Builder
	b.WriteString("下面是某个 GitHub 仓库的 issue 统计数据，请给出不超过 500 字的中文分析，")
	b.WriteString("聚焦：1) 当前主要矛盾；2) 维护健康度；3) 最值得先做的三件事。\n\n")
	b.WriteString(fmt.Sprintf("仓库：%s\n", data.Meta.FullName))
	b.WriteString(fmt.Sprintf("总数 %d，未关闭 %d，已关闭 %d，关闭率 %.1f%%\n",
		data.Summary.Total, data.Summary.Open, data.Summary.Closed, data.Summary.CloseRate))
	b.WriteString("标签分布：" + distText(data.LabelDist) + "\n")
	b.WriteString("创建者分布：" + distText(data.Creators) + "\n\n")
	b.WriteString("issue 标题列表：\n")
	for i, it := range data.Issues {
		if i >= 120 {
			b.WriteString(fmt.Sprintf("...（其余 %d 条略）\n", len(data.Issues)-i))
			break
		}
		b.WriteString(fmt.Sprintf("- #%d [%s] %s\n", it.Number, it.State, it.Title))
	}
	return callAI(b.String(), token, model, baseURL)
}

func aiDeepInsight(issues []*github.Issue, items []issueAnalysis, token, model, baseURL string) (string, error) {
	if len(issues) == 0 {
		return "", errors.New("没有 issue 可供分析")
	}
	var b strings.Builder
	b.WriteString("下面是某个 GitHub 仓库中被用户选中的 issue，请给出不超过 600 字的中文深度分析：")
	b.WriteString("问题根因归类、相互关联、处置优先级建议。\n\n")
	for i, it := range items {
		if i >= 40 {
			b.WriteString(fmt.Sprintf("...（其余 %d 条略）\n", len(items)-i))
			break
		}
		body := it.Title
		for _, src := range issues {
			if src.GetNumber() == it.Number {
				if src.GetBody() != "" {
					body = src.GetTitle() + "\n" + truncateRunes(src.GetBody(), 800)
				}
				break
			}
		}
		b.WriteString(fmt.Sprintf("### #%d [%s/%s] %s\n%s\n\n", it.Number, it.State, it.Category, it.Title, body))
	}
	return callAI(b.String(), token, model, baseURL)
}

// callAI 调用 OpenAI 兼容接口；失败时返回 error，由调用方降级到本地统计。
func callAI(prompt, token, model, baseURL string) (string, error) {
	if token == "" {
		return "", errors.New("未提供 AI Token")
	}
	llm, err := newLLMClient(token, model, baseURL)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), aiTimeout)
	defer cancel()

	out, err := generateFromPrompt(ctx, llm, prompt)
	if err != nil {
		return "", fmt.Errorf("AI 请求失败: %w", err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", errors.New("AI 返回内容为空")
	}
	return out, nil
}

func truncateRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}

// ---------- 辅助 ----------

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if r.Body == nil {
		return errors.New("请求体为空")
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodySize))
	if err != nil {
		return errors.New("请求体过大或读取失败")
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return errors.New("请求体为空")
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "序列化响应失败")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(raw)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// ---------- 启动 ----------

// runWebServer 启动 Web 服务，阻塞直到收到退出信号。
func runWebServer(o webOptions) error {
	srv, err := newWebServer(o)
	if err != nil {
		return err
	}
	handler := srv.routes()

	ln, port, err := listenWithFallback(o.Port)
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:              ln.Addr().String(),
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	home := "http://" + addr + "/"
	fmt.Println()
	fmt.Println("issue2file Web 模式已启动")
	fmt.Printf("  打开页面： %s\n", home)
	if o.GitHubToken != "" {
		fmt.Println("  已加载服务端 GitHub Token，私有仓库无需在页面重复填写")
	} else {
		fmt.Println("  未配置 GitHub Token：公开仓库可直接用，私有仓库请在页面填 Token")
	}
	fmt.Println("  按 Ctrl+C 退出")
	fmt.Println()

	if o.OpenBrowser {
		openBrowser(home)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		return err
	case <-sigCh:
		log.Info("收到退出信号，正在关闭 Web 服务...")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		return fmt.Errorf("关闭 Web 服务失败: %w", err)
	}
	return nil
}

// listenWithFallback 优先用指定端口，被占用时向后尝试 20 个端口。
func listenWithFallback(port int) (net.Listener, int, error) {
	if port <= 0 {
		port = 8080
	}
	var lastErr error
	for i := 0; i < 20; i++ {
		p := port + i
		if p > 65535 {
			break
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			return ln, p, nil
		}
		lastErr = err
	}
	return nil, 0, fmt.Errorf("无法监听端口（从 %d 开始尝试了 20 个）: %w", port, lastErr)
}

func openBrowser(target string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	// 只启动不等待，避免阻塞 Web 服务
	if err := cmd.Start(); err != nil {
		log.Debugf("自动打开浏览器失败: %v", err)
		return
	}
	go func() { _ = cmd.Wait() }()
}
