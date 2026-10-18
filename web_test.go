package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v57/github"
)

// ---------- 测试用 GitHub API 替身 ----------

func webIssue(number int, state, title, body, created, closed string, pr bool, labels ...string) *github.Issue {
	i := &github.Issue{
		Number:    github.Int(number),
		State:     github.String(state),
		Title:     github.String(title),
		Body:      github.String(body),
		CreatedAt: &github.Timestamp{Time: mustTime(created)},
		User:      &github.User{Login: github.String("alice")},
		HTMLURL:   github.String(fmt.Sprintf("https://github.com/o/r/issues/%d", number)),
		Comments:  github.Int(0),
	}
	if closed != "" {
		i.ClosedAt = &github.Timestamp{Time: mustTime(closed)}
	}
	if pr {
		i.PullRequestLinks = &github.PullRequestLinks{URL: github.String("https://api.github.com/o/r/pulls/1")}
	}
	for _, l := range labels {
		i.Labels = append(i.Labels, &github.Label{Name: github.String(l)})
	}
	return i
}

func mustTime(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

// fakeGitHub 起一个最小可用的 GitHub REST 替身，让测试完全不依赖外网。
type fakeGitHub struct {
	owner    string
	repo     string
	issues   []*github.Issue
	comments map[int][]*github.IssueComment
	// notFound 为真时所有接口都返回 404，用来模拟私有仓库 / 仓库不存在
	notFound bool
	// requireToken 为真时没有 Authorization 头的请求一律 401，
	// 用来验证「填了 Token 就能访问私有仓库」这条需求真的成立
	requireToken bool
	// issuesForbidden 为真时分页拉取 issue 的接口返回 403，
	// 用来覆盖最容易触发限流的那一步（而不是只有仓库信息那一步）
	issuesForbidden bool
	// seenAuth 记录替身实际收到的 Authorization 头，用于断言 Token 确实被带过去了
	seenAuth []string
}

func (f *fakeGitHub) start(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	base := "/repos/" + f.owner + "/" + f.repo

	if f.notFound {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		})
		ts := httptest.NewServer(mux)
		t.Cleanup(ts.Close)
		return ts
	}

	authOK := func(w http.ResponseWriter, r *http.Request) bool {
		auth := r.Header.Get("Authorization")
		f.seenAuth = append(f.seenAuth, auth)
		if f.requireToken && !strings.HasPrefix(auth, "Bearer ") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Requires authentication"}`))
			return false
		}
		return true
	}

	mux.HandleFunc(base, func(w http.ResponseWriter, r *http.Request) {
		if !authOK(w, r) {
			return
		}
		writeFakeJSON(w, map[string]any{
			"full_name":         f.owner + "/" + f.repo,
			"name":              f.repo,
			"description":       "测试仓库",
			"html_url":          "https://github.com/" + f.owner + "/" + f.repo,
			"stargazers_count":  42,
			"forks_count":       7,
			"subscribers_count": 5,
			"open_issues_count": 2,
			"language":          "Go",
			"license":           map[string]any{"key": "mit", "name": "MIT", "spdx_id": "MIT"},
			"default_branch":    "main",
			"topics":            []string{"go", "issues"},
			"private":           false,
			"created_at":        "2023-01-01T00:00:00Z",
			"updated_at":        "2024-06-01T00:00:00Z",
		})
	})
	mux.HandleFunc(base+"/issues", func(w http.ResponseWriter, r *http.Request) {
		if !authOK(w, r) {
			return
		}
		if f.issuesForbidden {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"API rate limit exceeded for 127.0.0.1"}`))
			return
		}
		page := r.URL.Query().Get("page")
		if page != "" && page != "1" && page != "0" {
			writeFakeJSON(w, []*github.Issue{})
			return
		}
		writeFakeJSON(w, f.issues)
	})
	mux.HandleFunc(base+"/issues/", func(w http.ResponseWriter, r *http.Request) {
		if !authOK(w, r) {
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, base+"/issues/")
		num := 0
		if _, err := fmt.Sscanf(rest, "%d/comments", &num); err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeFakeJSON(w, f.comments[num])
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func writeFakeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	raw, err := json.Marshal(v)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(raw)
}

func testServer(t *testing.T, apiBase string) *webServer {
	t.Helper()
	srv, err := newWebServer(webOptions{APIBaseURL: apiBase})
	if err != nil {
		t.Fatalf("创建 Web 服务失败: %v", err)
	}
	return srv
}

func postJSON(t *testing.T, srv *webServer, path string, body any, out any) int {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化请求失败: %v", err)
	}
	req := localRequest(http.MethodPost, path, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if out != nil && rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("响应不是合法 JSON(%d): %v / %s", rec.Code, err, rec.Body.String())
		}
	}
	return rec.Code
}

// localRequest 构造一个带本地 Host 的请求。
// httptest.NewRequest 默认 Host 是 example.com，会被服务端的本机访问校验挡掉；
// 这里显式置成 127.0.0.1，让测试走真实请求链路（含这道校验）。
func localRequest(method, path string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, path, body)
	req.Host = "127.0.0.1:8080"
	return req
}

type errBody struct {
	Error string `json:"error"`
}

// ---------- 页面与配置 ----------

func TestIndexPage_ServedWithMarkers(t *testing.T) {
	srv := testServer(t, "")
	req := localRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", rec.Code)
	}
	body := rec.Body.String()
	for _, marker := range []string{"issue2file", "/api/fetch", "/api/analyze", "/api/export", "repoInput"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("页面缺少标记 %q", marker)
		}
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("首页 Content-Type 不对: %s", rec.Header().Get("Content-Type"))
	}
}

func TestIndexPage_UnknownPathIs404(t *testing.T) {
	srv := testServer(t, "")
	req := localRequest(http.MethodGet, "/nope", nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知路径应返回 404，实际 %d", rec.Code)
	}
}

func TestHandleConfig_ReportsTokenState(t *testing.T) {
	srv, err := newWebServer(webOptions{GitHubToken: "ghp_x", AIToken: "", AIModel: "m1"})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	req := localRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	var got struct {
		HasGitHubToken bool   `json:"hasGitHubToken"`
		HasAIToken     bool   `json:"hasAIToken"`
		AIModel        string `json:"aiModel"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !got.HasGitHubToken || got.HasAIToken || got.AIModel != "m1" {
		t.Fatalf("配置接口返回不正确: %+v", got)
	}
}

// ---------- /api/fetch ----------

func TestHandleFetch_SuccessAndCreatesSession(t *testing.T) {
	fake := &fakeGitHub{
		owner: "o", repo: "r",
		issues: []*github.Issue{
			webIssue(1, "open", "登录接口报错 500", "调用 /login 时返回 panic 堆栈", "2024-01-01", "", false, "bug"),
			webIssue(2, "closed", "希望支持导出 CSV", "希望支持导出 CSV 格式", "2024-02-01", "2024-02-10", false, "enhancement"),
			webIssue(3, "open", "chore: bump deps", "bump deps", "2024-03-01", "", true), // PR，默认应被过滤
		},
	}
	srv := testServer(t, fake.start(t).URL+"/")

	var got fetchResponse
	code := postJSON(t, srv, "/api/fetch", fetchRequest{Repo: "https://github.com/o/r"}, &got)
	if code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", code)
	}
	if got.SessionID == "" {
		t.Fatal("未返回 sessionId，后续无法做选中分析")
	}
	if got.Repo.FullName != "o/r" || got.Repo.Stars != 42 || got.Repo.Language != "Go" {
		t.Fatalf("仓库信息不正确: %+v", got.Repo)
	}
	if got.Data.Summary.Total != 2 {
		t.Fatalf("PR 应被过滤，期望 2 个 issue，实际 %d", got.Data.Summary.Total)
	}
	if !strings.Contains(got.Report, "初步分析报告") || !strings.Contains(got.Report, "登录接口报错") {
		t.Fatalf("报告内容不完整: %s", got.Report[:min(len(got.Report), 300)])
	}
	if len(got.Data.Issues) != 2 {
		t.Fatalf("issue 列表应为 2 条，实际 %d", len(got.Data.Issues))
	}
}

func TestHandleFetch_IncludePR(t *testing.T) {
	fake := &fakeGitHub{
		owner: "o", repo: "r",
		issues: []*github.Issue{
			webIssue(1, "open", "issue one", "body", "2024-01-01", "", false),
			webIssue(2, "open", "pr one", "body", "2024-01-02", "", true),
		},
	}
	srv := testServer(t, fake.start(t).URL+"/")

	var got fetchResponse
	code := postJSON(t, srv, "/api/fetch", fetchRequest{Repo: "o/r", IncludePR: true}, &got)
	if code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", code)
	}
	if got.Data.Summary.Total != 2 {
		t.Fatalf("勾选包含 PR 后应为 2，实际 %d", got.Data.Summary.Total)
	}
}

func TestHandleFetch_PrivateRepoWithoutToken(t *testing.T) {
	fake := &fakeGitHub{owner: "o", repo: "r", notFound: true}
	srv := testServer(t, fake.start(t).URL+"/")

	var got errBody
	code := postJSON(t, srv, "/api/fetch", fetchRequest{Repo: "o/r"}, &got)
	// 仓库不存在 / 无权限是 GitHub 返回的 404，应当原样透传而不是谎报成 400
	if code != http.StatusNotFound {
		t.Fatalf("期望 404，实际 %d", code)
	}
	if !strings.Contains(got.Error, "私有仓库") || !strings.Contains(got.Error, "Token") {
		t.Fatalf("404 场景应提示填写 Token，实际: %s", got.Error)
	}
}

func TestHandleFetch_EmptyRepoStillReturns200(t *testing.T) {
	fake := &fakeGitHub{owner: "o", repo: "r", issues: []*github.Issue{}}
	srv := testServer(t, fake.start(t).URL+"/")

	var got fetchResponse
	code := postJSON(t, srv, "/api/fetch", fetchRequest{Repo: "o/r"}, &got)
	if code != http.StatusOK {
		t.Fatalf("空仓库不应报错，实际 %d", code)
	}
	if got.Data.Summary.Total != 0 {
		t.Fatalf("空仓库总数应为 0，实际 %d", got.Data.Summary.Total)
	}
	if len(got.Warnings) == 0 {
		t.Fatal("空仓库应给出提示，前端才能解释发生了什么")
	}
}

func TestHandleFetch_InvalidInputs(t *testing.T) {
	srv := testServer(t, "")

	cases := []struct {
		name string
		body fetchRequest
		want string
	}{
		{"空仓库地址", fetchRequest{Repo: "  "}, "请填写仓库地址"},
		{"非法地址", fetchRequest{Repo: "https://gitlab.com/a/b/c"}, "无法解析仓库地址"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got errBody
			code := postJSON(t, srv, "/api/fetch", c.body, &got)
			if code != http.StatusBadRequest {
				t.Fatalf("期望 400，实际 %d", code)
			}
			if !strings.Contains(got.Error, c.want) {
				t.Fatalf("错误信息不含 %q: %s", c.want, got.Error)
			}
		})
	}
}

func TestHandleFetch_MethodNotAllowed(t *testing.T) {
	srv := testServer(t, "")
	req := localRequest(http.MethodGet, "/api/fetch", nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("期望 405，实际 %d", rec.Code)
	}
}

func TestHandleFetch_BadJSON(t *testing.T) {
	srv := testServer(t, "")
	req := localRequest(http.MethodPost, "/api/fetch", strings.NewReader("{not json"))
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 JSON 应返回 400，实际 %d", rec.Code)
	}
}

// ---------- /api/analyze ----------

func analyzeSetup(t *testing.T) (*webServer, string) {
	t.Helper()
	one := webIssue(1, "open", "登录接口报错 500，生产环境不可用", "调用 /login 返回 panic\n```\ngoroutine 1\n```", "2023-01-01", "", false, "bug")
	// 真实接口会带上评论数；为 0 时后端会跳过评论请求，这里显式置 1 才会去拉评论
	one.Comments = github.Int(1)
	fake := &fakeGitHub{
		owner: "o", repo: "r",
		issues: []*github.Issue{
			one,
			webIssue(2, "closed", "希望新增导出 CSV 的能力", "希望新增导出 CSV 的能力，方便二次分析", "2024-02-01", "2024-02-05", false, "enhancement"),
			webIssue(3, "open", "文档里示例跑不通", "文档里示例跑不通，按 README 操作会报错", "2024-03-01", "", false, "documentation"),
		},
		comments: map[int][]*github.IssueComment{
			1: {{User: &github.User{Login: github.String("bob")}, Body: github.String("我也遇到了，是数据库连接池配置问题")}},
		},
	}
	srv := testServer(t, fake.start(t).URL+"/")
	var fr fetchResponse
	if code := postJSON(t, srv, "/api/fetch", fetchRequest{Repo: "o/r"}, &fr); code != http.StatusOK || fr.SessionID == "" {
		t.Fatalf("准备会话失败: code=%d", code)
	}
	return srv, fr.SessionID
}

func TestHandleAnalyze_SelectedIssues(t *testing.T) {
	srv, sid := analyzeSetup(t)

	var got analyzeResponse
	code := postJSON(t, srv, "/api/analyze", analyzeRequest{
		SessionID:    sid,
		Numbers:      []int{1, 2},
		WithComments: true,
	}, &got)
	if code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", code)
	}
	if got.Count != 2 || len(got.Items) != 2 {
		t.Fatalf("应分析 2 个 issue，实际 count=%d items=%d", got.Count, len(got.Items))
	}

	first := got.Items[0]
	if first.Number != 1 {
		t.Fatalf("结果顺序应按传入顺序，实际首个是 #%d", first.Number)
	}
	if first.Category != catBug {
		t.Fatalf("#1 应被归类为 %s，实际 %s", catBug, first.Category)
	}
	if !first.HasStack {
		t.Fatal("#1 正文含 goroutine 堆栈，应被识别")
	}
	if first.Priority != "P0" && first.Priority != "P1" {
		t.Fatalf("#1 含紧急信号且长期未关闭，优先级应更高，实际 %s(%d)", first.Priority, first.Score)
	}
	if len(first.Keywords) == 0 {
		t.Fatal("#1 应能抽出关键词")
	}
	if first.Participants < 2 {
		t.Fatalf("#1 有评论者，参与人数应 >=2，实际 %d", first.Participants)
	}
	if !strings.Contains(got.Report, "#1") || !strings.Contains(got.Report, "深度分析报告") {
		t.Fatalf("深度报告内容不完整: %s", got.Report[:min(len(got.Report), 200)])
	}
	if len(got.CategoryDist) == 0 || len(got.PriorityDist) == 0 || len(got.AgeDist) == 0 {
		t.Fatalf("缺少聚合分布: %+v", got)
	}
}

func TestHandleAnalyze_ErrorPaths(t *testing.T) {
	srv, sid := analyzeSetup(t)

	cases := []struct {
		name string
		body analyzeRequest
		want string
	}{
		{"未知会话", analyzeRequest{SessionID: "not-exist", Numbers: []int{1}}, "会话已过期"},
		{"空会话", analyzeRequest{Numbers: []int{1}}, "会话已过期"},
		{"未选 issue", analyzeRequest{SessionID: sid}, "至少选择一个"},
		{"选中不存在", analyzeRequest{SessionID: sid, Numbers: []int{9999}}, "不在当前会话"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got errBody
			code := postJSON(t, srv, "/api/analyze", c.body, &got)
			if code != http.StatusBadRequest {
				t.Fatalf("期望 400，实际 %d", code)
			}
			if !strings.Contains(got.Error, c.want) {
				t.Fatalf("错误信息不含 %q: %s", c.want, got.Error)
			}
		})
	}
}

func TestHandleAnalyze_DuplicateNumbersDeduplicated(t *testing.T) {
	srv, sid := analyzeSetup(t)
	var got analyzeResponse
	code := postJSON(t, srv, "/api/analyze", analyzeRequest{SessionID: sid, Numbers: []int{1, 1, 2}}, &got)
	if code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", code)
	}
	if got.Count != 2 {
		t.Fatalf("重复编号应去重，期望 2，实际 %d", got.Count)
	}
}

// ---------- /api/export ----------

func TestHandleExport_AllAndSelected(t *testing.T) {
	srv, sid := analyzeSetup(t)

	var all exportResponse
	if code := postJSON(t, srv, "/api/export", exportRequest{SessionID: sid}, &all); code != http.StatusOK {
		t.Fatalf("导出全部失败: %d", code)
	}
	if !strings.HasSuffix(all.Filename, ".md") {
		t.Fatalf("导出文件名应以 .md 结尾: %s", all.Filename)
	}
	for _, want := range []string{"Issue #1", "Issue #2", "Issue #3"} {
		if !strings.Contains(all.Content, want) {
			t.Fatalf("导出内容缺少 %q", want)
		}
	}

	var sel exportResponse
	if code := postJSON(t, srv, "/api/export", exportRequest{SessionID: sid, Numbers: []int{2}, Report: "## 我的报告\n"}, &sel); code != http.StatusOK {
		t.Fatalf("导出选中失败: %d", code)
	}
	if strings.Contains(sel.Content, "Issue #1") {
		t.Fatal("只选了 #2，不应包含 #1")
	}
	if !strings.Contains(sel.Content, "我的报告") || !strings.Contains(sel.Content, "Issue #2") {
		t.Fatalf("导出内容应包含传入的报告与选中的 issue: %s", sel.Content[:min(len(sel.Content), 200)])
	}
}

func TestHandleExport_BadSession(t *testing.T) {
	srv := testServer(t, "")
	var got errBody
	code := postJSON(t, srv, "/api/export", exportRequest{SessionID: "nope"}, &got)
	if code != http.StatusBadRequest || !strings.Contains(got.Error, "会话已过期") {
		t.Fatalf("期望 400 且提示会话过期，实际 %d / %s", code, got.Error)
	}
}

// ---------- 会话存储 ----------

func TestSessionStore_GetRefreshesAndExpires(t *testing.T) {
	store := newSessionStore(50*time.Millisecond, 10)
	sess := store.Put("o", "r", "tok", nil)
	if store.Get(sess.ID) == nil {
		t.Fatal("刚写入的会话应可取到")
	}
	time.Sleep(80 * time.Millisecond)
	if store.Get(sess.ID) != nil {
		t.Fatal("超过 TTL 的会话应失效")
	}
	if store.Get("") != nil {
		t.Fatal("空 ID 不应命中任何会话")
	}
}

func TestSessionStore_EvictsOldest(t *testing.T) {
	store := newSessionStore(time.Hour, 2)
	a := store.Put("o", "a", "", nil)
	b := store.Put("o", "b", "", nil)
	c := store.Put("o", "c", "", nil)

	if store.Get(a.ID) != nil {
		t.Fatal("超出容量后最旧的会话应被淘汰")
	}
	if store.Get(b.ID) == nil || store.Get(c.ID) == nil {
		t.Fatal("较新的会话不应被淘汰")
	}
}

// ---------- 纯函数 ----------

func TestClassifyIssue(t *testing.T) {
	cases := []struct {
		title  string
		labels []string
		body   string
		want   string
	}{
		{"panic when parsing config", nil, "goroutine 1 [running]", catBug},
		{"希望支持批量导出", []string{"enhancement"}, "", catFeature},
		{"请问这个参数怎么配置", nil, "如题", catQuestion},
		{"README 里的示例旧了", nil, "文档过时", catDocs},
		{"bump dependencies", nil, "upgrade go mod", catChore},
		{"随便聊聊", nil, "今天天气不错", catOther},
	}
	for _, c := range cases {
		got, _ := classifyIssue(c.title, c.labels, c.body)
		if got != c.want {
			t.Fatalf("标题 %q 期望 %s，实际 %s", c.title, c.want, got)
		}
	}
}

func TestPriorityOf_UrgentRanksHigher(t *testing.T) {
	urgent, s1, _ := priorityOf("open", 400, 12, catBug, "生产环境崩溃 严重", "", false, true)
	normal, s2, _ := priorityOf("closed", 2, 0, catFeature, "加个小按钮", "", true, false)
	if s1 <= s2 {
		t.Fatalf("紧急缺陷应得分更高，实际 %d vs %d", s1, s2)
	}
	if urgent != "P0" {
		t.Fatalf("紧急缺陷应为 P0，实际 %s", urgent)
	}
	if normal != "P3" {
		t.Fatalf("低风险已关闭需求应为 P3，实际 %s(%d)", normal, s2)
	}
}

func TestPriorityOf_AlwaysHasReason(t *testing.T) {
	_, _, reasons := priorityOf("closed", 0, 0, catOther, "x", "", true, false)
	if len(reasons) == 0 {
		t.Fatal("任何 issue 都应给出至少一条依据，否则页面会出现空白区块")
	}
}

func TestExtractKeywords(t *testing.T) {
	got := extractKeywords("配置文件解析失败",
		"配置文件解析失败，配置文件解析失败时会 panic，配置文件的格式需要校验。", 8)
	if len(got) == 0 {
		t.Fatal("中文正文应能抽出关键词")
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "配置") {
		t.Fatalf("关键词应包含「配置」，实际 %v", got)
	}

	en := extractKeywords("redis connection pool",
		"redis connection pool exhausted, redis connection pool exhausted again", 5)
	if len(en) == 0 || !strings.Contains(strings.Join(en, " "), "redis") {
		t.Fatalf("英文关键词抽取失败: %v", en)
	}
}

func TestExtractKeywords_EmptyAndShort(t *testing.T) {
	if got := extractKeywords("", "", 5); len(got) != 0 {
		t.Fatalf("空输入应返回空切片，实际 %v", got)
	}
	// 只出现一次的词噪声太大，不应上榜
	if got := extractKeywords("", "abcdefg hijklmn", 5); len(got) != 0 {
		t.Fatalf("低频噪声词不应上榜，实际 %v", got)
	}
}

func TestDeepAnalyze_EmptyInputIsSafe(t *testing.T) {
	items, cat, prio, kw, age := deepAnalyze(nil, time.Now(), nil)
	if len(items) != 0 || len(cat) != 0 || len(prio) != 0 || len(kw) != 0 || len(age) != 0 {
		t.Fatal("空输入应产出空结果而不是 panic")
	}
}

func TestDeepAnalyze_CountsAndBuckets(t *testing.T) {
	now := time.Now()
	issues := []*github.Issue{
		webIssue(1, "open", "崩溃了 panic", "panic", now.AddDate(-1, 0, 0).Format("2006-01-02"), "", false, "bug"),
		webIssue(2, "closed", "希望支持导出", "希望支持导出", "2024-01-01", "2024-01-02", false, "enhancement"),
	}
	items, cat, prio, kw, age := deepAnalyze(issues, now, nil)
	if len(items) != 2 {
		t.Fatalf("期望 2 条，实际 %d", len(items))
	}
	if len(cat) != 2 {
		t.Fatalf("分类应有两种，实际 %v", cat)
	}
	if len(age) != 2 {
		t.Fatalf("耗时分布应有两组（未关闭 / 1 天内），实际 %v", age)
	}
	found := false
	for _, a := range age {
		if a.Name == "1 天内" {
			found = true
		}
	}
	if !found {
		t.Fatalf("缺少「1 天内」分桶: %v", age)
	}
	if len(prio) == 0 {
		t.Fatal("优先级分布不应为空")
	}
	_ = kw
}

func TestBuildOverviewReport_KeySections(t *testing.T) {
	issues := []*github.Issue{
		webIssue(1, "open", "登录报错 panic", "panic", "2024-01-01", "", false, "bug"),
		webIssue(2, "closed", "希望支持导出", "希望支持导出", "2024-02-01", "2024-02-11", false, "enhancement"),
	}
	data := BuildDashboardData("o", "r", issues)
	report := BuildOverviewReport(data, repoInfo{FullName: "o/r", Description: "测试仓库"})

	for _, want := range []string{"初步分析报告", "核心指标", "状态分布", "月度趋势", "需要重点关注", "行动建议", "o/r"} {
		if !strings.Contains(report, want) {
			t.Fatalf("报告缺少章节 %q", want)
		}
	}
	if !strings.Contains(report, "10.0 天") {
		t.Fatalf("平均关闭耗时应为 10.0 天，报告片段: %s", report)
	}
}

func TestBuildOverviewReport_EmptyDataNoPanic(t *testing.T) {
	data := BuildDashboardData("o", "r", nil)
	report := BuildOverviewReport(data, repoInfo{})
	if !strings.Contains(report, "初步分析报告") {
		t.Fatal("空数据也要产出报告骨架")
	}
	if !strings.Contains(report, "暂无数据") {
		t.Fatalf("空数据应在表格处说明暂无数据: %s", report)
	}
}

func TestBuildDeepReport_ContainsEverySelected(t *testing.T) {
	items := []issueAnalysis{
		{Number: 7, Title: "一个缺陷", URL: "https://x/7", State: "open", Category: catBug, Priority: "P1", Score: 5,
			Days: 30, Reasons: []string{"未关闭"}, Keywords: []string{"配置"}},
		{Number: 8, Title: "一个需求", URL: "https://x/8", State: "closed", Category: catFeature, Priority: "P3", Score: 1,
			Days: 2, Reasons: []string{"低风险"}, Keywords: []string{"导出"}},
	}
	report := BuildDeepReport(items, Meta{FullName: "o/r", GeneratedAt: "2024-01-01 00:00:00"},
		[]NameValue{{Name: catBug, Value: 1}, {Name: catFeature, Value: 1}},
		[]NameValue{{Name: "P1", Value: 1}, {Name: "P3", Value: 1}},
		[]NameValue{{Name: "配置", Value: 1}},
		[]NameValue{{Name: "未关闭", Value: 1}}, "AI 说：先修 #7")

	for _, want := range []string{"#7", "#8", "AI 说", "处置顺序建议", "P1"} {
		if !strings.Contains(report, want) {
			t.Fatalf("深度报告缺少 %q", want)
		}
	}
}

func TestPickIssues_DedupeKeepsOrder(t *testing.T) {
	all := []*github.Issue{
		webIssue(1, "open", "a", "", "2024-01-01", "", false),
		webIssue(2, "open", "b", "", "2024-01-01", "", false),
	}
	got := pickIssues(all, []int{2, 1, 2, 99})
	if len(got) != 2 {
		t.Fatalf("应按去重后返回 2 条，实际 %d", len(got))
	}
	if got[0].GetNumber() != 2 || got[1].GetNumber() != 1 {
		t.Fatalf("应保持传入顺序，实际 %d,%d", got[0].GetNumber(), got[1].GetNumber())
	}
}

func TestSafeFileName(t *testing.T) {
	if got := safeFileName("issues_o_r.md"); got != "issues_o_r.md" {
		t.Fatalf("正常文件名不应被改动: %s", got)
	}
	if got := safeFileName("../../etc/passwd"); strings.Contains(got, "/") {
		t.Fatalf("路径分隔符必须被替换: %s", got)
	}
}

func TestBuildExportMarkdown_HeaderAndBody(t *testing.T) {
	issues := []*github.Issue{webIssue(5, "open", "标题", "正文", "2024-01-01", "", false)}
	out := buildExportMarkdown("o", "r", issues, nil, "## 报告\n", []string{"提示"})
	for _, want := range []string{"o/r", "报告", "Issue #5", "提示"} {
		if !strings.Contains(out, want) {
			t.Fatalf("导出内容缺少 %q", want)
		}
	}
}

func TestNewGitHubClient_BaseURLOverride(t *testing.T) {
	c, err := newGitHubClient("tok", "https://example.com/api/v3/")
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	if c.BaseURL.String() != "https://example.com/api/v3/" {
		t.Fatalf("BaseURL 未生效: %s", c.BaseURL.String())
	}
	c2, err := newGitHubClient("", "")
	if err != nil {
		t.Fatalf("匿名客户端创建失败: %v", err)
	}
	if c2.BaseURL.Host != "api.github.com" {
		t.Fatalf("默认应指向官方 API，实际 %s", c2.BaseURL.String())
	}
}

func TestListenWithFallback_OccupiedPort(t *testing.T) {
	// 先占住一个端口，再要求从该端口开始监听，应当顺延到下一个
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("无法获取空闲端口: %v", err)
	}
	busy := ln.Addr().(*net.TCPAddr).Port
	defer ln.Close()

	got, port, err := listenWithFallback(busy)
	if err != nil {
		t.Fatalf("端口被占用时应顺延而不是报错: %v", err)
	}
	defer got.Close()
	if port == busy {
		t.Fatalf("不应复用被占用的端口 %d", busy)
	}
}

func TestMedianCloseDays(t *testing.T) {
	issues := []IssueView{
		{State: "closed", Days: intPtr(10)},
		{State: "closed", Days: intPtr(2)},
		{State: "closed", Days: intPtr(20)},
		{State: "open", Days: intPtr(999)},
	}
	if got := medianCloseDays(issues); got != 10 {
		t.Fatalf("中位数应为 10，实际 %v", got)
	}
	if got := medianCloseDays(nil); got != 0 {
		t.Fatalf("无已关闭 issue 时应为 0，实际 %v", got)
	}
}

func intPtr(v int) *int { return &v }

func TestFriendlyGitHubError_NotFound(t *testing.T) {
	// 用一个 404 的 ErrorResponse 走一遍翻译逻辑
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer ts.Close()

	c, err := newGitHubClient("", ts.URL+"/")
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	_, _, err = c.Repositories.Get(context.Background(), "o", "r")
	if err == nil {
		t.Fatal("期望拿到错误")
	}
	msg := friendlyGitHubError(err, "o/r").Error()
	if !strings.Contains(msg, "私有仓库") {
		t.Fatalf("404 应提示私有仓库填 Token，实际: %s", msg)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------- 本机访问校验 ----------

func requestWithHost(host string) *http.Request {
	req := localRequest(http.MethodGet, "/api/config", nil)
	req.Host = host
	return req
}

func TestLocalGuard_RejectsForeignHost(t *testing.T) {
	srv := testServer(t, "")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, requestWithHost("evil.example.com"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("非本机 Host 应被拒绝，实际 %d", rec.Code)
	}
}

func TestLocalGuard_AllowsLoopbackHosts(t *testing.T) {
	srv := testServer(t, "")
	for _, host := range []string{"127.0.0.1:8080", "localhost:9000", "[::1]:1234"} {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, requestWithHost(host))
		if rec.Code != http.StatusOK {
			t.Fatalf("回环地址 %s 应放行，实际 %d", host, rec.Code)
		}
	}
}

func TestLocalGuard_RejectsCrossSiteOrigin(t *testing.T) {
	srv := testServer(t, "")
	req := localRequest(http.MethodGet, "/api/config", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("跨站 Origin 应被拒绝，实际 %d", rec.Code)
	}
}

func TestIndexPage_SecurityHeadersAndNonce(t *testing.T) {
	srv := testServer(t, "")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, localRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("首页缺少 CSP 响应头")
	}
	if strings.Contains(csp, "unsafe-inline") {
		t.Fatalf("CSP 不应开放 unsafe-inline，否则等于没加: %s", csp)
	}
	// 内联 <script> 带了 nonce 才能在不开放 unsafe-inline 的前提下执行
	nonce := ""
	if i := strings.Index(csp, "'nonce-"); i >= 0 {
		rest := csp[i+len("'nonce-"):]
		nonce = rest[:strings.Index(rest, "'")]
	}
	if nonce == "" {
		t.Fatalf("CSP 里没找到 nonce: %s", csp)
	}
	if !strings.Contains(body, `nonce="`+nonce+`"`) {
		t.Fatal("内联 script/style 没有带上对应的 nonce")
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("缺少 X-Content-Type-Options")
	}
}

// ---------- Token 链路 ----------

func TestHandleFetch_TokenReachesGitHubAndIsNotEchoedBack(t *testing.T) {
	const secret = "ghp_SECRET123"
	fake := &fakeGitHub{owner: "o", repo: "r", requireToken: true,
		issues: []*github.Issue{webIssue(1, "open", "私有 issue", "内容不外传", "2024-01-01", "", false)}}
	srv := testServer(t, fake.start(t).URL+"/")

	raw, err := json.Marshal(fetchRequest{Repo: "o/r", Token: secret})
	if err != nil {
		t.Fatal(err)
	}
	req := localRequest(http.MethodPost, "/api/fetch", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("带 Token 访问私有仓库应成功，实际 %d / %s", rec.Code, rec.Body.String())
	}
	// Token 必须真的到达 GitHub，否则「填了 Token 就能拉私有仓库」这条需求没有任何保障
	if len(fake.seenAuth) == 0 || !strings.HasPrefix(fake.seenAuth[0], "Bearer "+secret) {
		t.Fatalf("GitHub 替身没收到正确的 Authorization: %+v", fake.seenAuth)
	}
	// 反过来，Token 不能被回显给用户
	if strings.Contains(rec.Body.String(), secret) || strings.Contains(rec.Header().Get("Set-Cookie"), secret) {
		t.Fatal("Token 泄漏到了响应中")
	}
}

func TestHandleFetch_WithoutTokenPrivateRepoFails(t *testing.T) {
	fake := &fakeGitHub{owner: "o", repo: "r", requireToken: true}
	srv := testServer(t, fake.start(t).URL+"/")

	var got errBody
	code := postJSON(t, srv, "/api/fetch", fetchRequest{Repo: "o/r"}, &got)
	if code != http.StatusUnauthorized {
		t.Fatalf("无 Token 访问私有仓库应返回 401，实际 %d", code)
	}
	if !strings.Contains(got.Error, "Token") {
		t.Fatalf("应提示 Token 无效，实际: %s", got.Error)
	}
}

// ---------- 分页阶段的错误翻译 ----------

func TestHandleFetch_Issues403IsTranslated(t *testing.T) {
	fake := &fakeGitHub{owner: "o", repo: "r", issuesForbidden: true}
	srv := testServer(t, fake.start(t).URL+"/")

	var got errBody
	code := postJSON(t, srv, "/api/fetch", fetchRequest{Repo: "o/r"}, &got)
	if code != http.StatusForbidden {
		t.Fatalf("分页 403 应原样返回 403，实际 %d", code)
	}
	if got.Error == "" || strings.Contains(got.Error, "http://") || strings.Contains(got.Error, "API rate limit exceeded") {
		t.Fatalf("分页阶段的错误应被翻译成中文提示，实际: %s", got.Error)
	}
}

// ---------- 会话 LRU ----------

func TestSessionStore_LRUKeepsRecentlyUsed(t *testing.T) {
	store := newSessionStore(time.Hour, 2)
	a := store.Put("o", "a", "", nil).ID
	b := store.Put("o", "b", "", nil).ID

	// 先碰一下 b，再反复访问 a，使 a 成为最近使用、b 成为最久未用
	if store.Get(b) == nil {
		t.Fatal("b 不应消失")
	}
	for i := 0; i < 3; i++ {
		if store.Get(a) == nil {
			t.Fatal("a 不应消失")
		}
	}
	store.Put("o", "c", "", nil)

	if store.Get(a) == nil {
		t.Fatal("a 刚刚还在反复使用，不应被淘汰（FIFO 实现会误伤这里）")
	}
	if store.Get(b) != nil {
		t.Fatal("b 最久未使用，应当被淘汰")
	}
	store.mu.Lock()
	n := len(store.items)
	store.mu.Unlock()
	if n != 2 {
		t.Fatalf("容量上限应控制在 2，实际 %d", n)
	}
}

// ---------- 响应形状 ----------

// warnings 必须始终是数组而不是 null：
// 同一个结构体里 aiInsight 是 ""、ageDist 是 []，唯独 null 会让前端必须同时处理三种形态。
func TestResponses_NeverEmitNullWarnings(t *testing.T) {
	fake := &fakeGitHub{owner: "o", repo: "r",
		issues: []*github.Issue{webIssue(1, "open", "标题", "正文", "2024-01-01", "", false)}}
	srv, err := newWebServer(webOptions{APIBaseURL: fake.start(t).URL + "/", GitHubToken: "ghp_local"})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}

	var fr fetchResponse
	postJSON(t, srv, "/api/fetch", fetchRequest{Repo: "o/r"}, &fr)

	var ar analyzeResponse
	postJSON(t, srv, "/api/analyze", analyzeRequest{SessionID: fr.SessionID, Numbers: []int{1}}, &ar)

	// nil 切片会被 encoding/json 序列化成 null
	if fr.Warnings == nil {
		t.Fatal("/api/fetch 的 warnings 不应为 null 序列化")
	}
	if ar.Warnings == nil {
		t.Fatal("/api/analyze 的 warnings 不应为 null 序列化")
	}
}

// ---------- AI BaseURL 归一化 ----------

func TestNormalizeAIBaseURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://api.deepseek.com/v1", "https://api.deepseek.com/v1"},
		{"https://api.deepseek.com/v1/", "https://api.deepseek.com/v1"},
		{"https://api.deepseek.com/v1/chat/completions", "https://api.deepseek.com/v1"},
		{"https://api.deepseek.com/v1/chat/completions/", "https://api.deepseek.com/v1"},
		{"  https://api.example.com/v3  ", "https://api.example.com/v3"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeAIBaseURL(c.in); got != c.want {
			t.Fatalf("normalizeAIBaseURL(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}
