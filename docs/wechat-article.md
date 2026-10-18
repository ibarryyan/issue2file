<!--
Audience:    中文开源项目的维护者 / 参与者 / 团队 Leader，会用命令行、用过 GitHub Issue，但不熟悉 Go 生态；需要的是「能立刻跑起来」
Thesis:      The core idea is: issue2file 先把一个 GitHub 仓库的全部 Issue 拉成本地 Markdown 文件，再让 AI 总结和图表分析建立在这份确定的本地文件之上 —— 网页上的 Issue 是浏览对象，本地的 Issue 才是可处理的数据。
Format:      微信公众号文章 · 约 3200 字（不含代码块）· 简体中文
Assumptions: 以仓库 main 分支源码为准（Go 1.23、go-github v57、langchaingo v0.1.13、go-echarts v2）。文中所有运行结果与坑点均在 macOS + go1.23.5 环境下实测。
-->

# 仓库几百条 Issue 读不完？把它一次性搬回本地

> 备选标题：
> ① issue2file：把 GitHub Issue 变成本地文档，再交给 AI 读
> ② 别再翻网页了：一条命令导完整个仓库的 Issue

---

## 一、先说问题

假设你现在要做这几件事里的任意一件：给手里的开源项目写一份年度回顾、把仓库交接给下一位维护者、看看过去半年用户到底在反馈什么、或者单纯想把这个仓库的所有 Issue 丢给大模型，让它帮你总结。

你打开 GitHub 的 Issue 页面，会遇到三重麻烦：

- **一次性看不到全貌**：网页是分页浏览式的， Popular 仓库动辄几百上千条，往前翻三页你就已经忘了第一页在说什么。
- **没法离线、没法批量处理**：Issue 活在 GitHub 的页面里，你没法 `grep`，没法全文检索，也没法把它当成数据去做统计。
- **没法直接喂给 AI**：你得一条条复制粘贴，或者临时写脚本调 API —— 而这件事每次要用的时候都要重写一遍。

GitHub 自己的搜索语法很强，但它解决的是「找」，不是「拿」。中间缺了一步：**把散落在网页上的 Issue，变成你硬盘上的一组文件。**

issue2file 做的就是这一步，顺手把后面两步也做了。

> **一句话**：issue2file 把一个 GitHub 仓库的全部 Issue（含 PR 和评论）拉成本地 Markdown 文件，然后基于这份确定的本地数据，做 AI 总结和可视化图表。它是一个 Go 写的单文件可以编译成二进制的命令行工具，没有服务端、没有数据库、不需要登录。

---

## 二、30 秒跑出第一个结果

前置条件只有一个：**Go 1.23 或更高版本**（源码 `go.mod` 里声明 `go 1.23.0`，实测 go1.23.5 可直接编译）。

### 第 1 步：编译

```bash
git clone https://github.com/ibarryyan/issue2file
cd issue2file
go mod tidy
go build -o issue2file
```

不想编译也行，`go run .` 直接跑。编译完只有一个二进制文件，拷到 `$PATH` 里就是个全局命令。

### 第 2 步：准备一份配置文件（不要跳过）

```bash
cp config.example.conf config.cnf
```

然后编辑 `config.cnf`，至少确认这两行：

```toml
gitHubToken = ""          # 先留空也能跑，见下文「关于 Token」
aiEnable    = false       # 默认不动，先跑通基础导出
chartEnable = true
commentEnable = true
outputDir   = "issues_output"
```

### 第 3 步：跑

```bash
./issue2file -config=./config.cnf ibarryyan/issue2file
```

真实输出长这样：

```
已加载配置文件: ./config.cnf
config: &{GitHubToken: AIToken: AIModel:deepseek-chat ... CommentEnable:true AiEnable:false ChartEnable:true OutputDir:issues_output SummaryFile:summary.md}
正在获取仓库 ibarryyan/issue2file 的issues...
提示: 未提供GitHub Token，使用匿名访问（API限制较严格）
已保存 issue #1: add github token config file or param
完成！共保存了 1 个issues到目录: %!s(*string=0x14000044440)
正在生成图表...
图表生成完成，可在 issues_output/charts 目录查看
```

（最后一行那串 `%!s(*string=...)` 是个日志打印的小 bug，文件其实已经正常落盘了，只是它把变量地址打了出来 —— 实际目录是 `issues_output/`。详细见第六节。）

仓库地址有三种写法，都支持：

```bash
./issue2file -config=./config.cnf owner/repo                 # 简短格式
./issue2file -config=./config.cnf https://github.com/owner/repo
./issue2file -config=./config.cnf git@github.com:owner/repo.git
```

还有个很顺手的用法：在任意本地仓库目录里执行 `./issue2file -config=./config.cnf .`，它会读 `.git/config` 里的 `origin` 地址，自动认出当前仓库 —— 适合给自己正在维护的项目做定期归档。

---

## 三、它到底在干什么

整个工具的逻辑其实只有一条主干，其余都是挂在主干上的可选分支：

```
   命令行参数 / config.cnf
            │
            ▼
   解析仓库地址    owner/repo · HTTPS/SSH URL · "." 读 .git/config
            │
            ▼
   GitHub Issues API   （state=all 全量拉取，100 条/页自动翻页）
            │
            ├──────────────►  <输出目录>/*.md       每个 Issue 一个文件（主干）
            │
            ├── aiEnable ───►  把所有 Issue 拼成一张 Markdown 表格
            │                  → 发给大模型 → summary.md
            │
            └── chartEnable ─►  状态饼图 / 标签柱状图 / 时间趋势折线
                               → charts/index.html
```

值得注意的设计取舍：**三个分支共享同一份内存里的 Issue 列表**，也就是说它不是「导出完再读回来分析」，而是一次抓取、三种产物。好处是快，代价是 AI 和图表依赖一次运行，没法对已导出的旧文件重跑分析。

抓取用的是 GitHub 官方 REST API 的列出仓库 Issue 接口，这里有两个必须知道的行为：

1. `state=all`，所以**已关闭的 Issue 也会导出**，这正是做回顾和统计时最需要的部分。
2. 这个接口在 GitHub 的语义里 **Issue 包含 Pull Request**（PR 本质上是带 `pull_request` 字段的 Issue）。我用 `octocat/Spoon-Knife` 实测调这个接口，返回的前 5 条全是 PR。issue2file 目前没有把这层过滤掉，所以导出结果里会混入 PR —— 这是当前版本需要注意的默认行为。

---

## 四、三个开关：评论、AI 总结、图表

主干导出之外，三个功能开关本身互不依赖，可以任意组合。

```bash
./issue2file -config=./config.cnf owner/repo -comment   # 连带每条 Issue 的评论区一起导出
./issue2file -config=./config.cnf owner/repo -ai        # 让大模型写一份分析总结
./issue2file -config=./config.cnf owner/repo -chart     # 生成三张 ECharts 图表
```

**图表**是最容易出效果的一个。它会在 `<输出目录>/charts/` 下生成 4 个 HTML 文件，直接浏览器打开 `index.html` 即可：

- `status_chart.html` —— 状态分布饼图（open / closed 占比）
- `labels_chart.html` —— 标签分布柱状图（按数量取前 10）
- `timeline_chart.html` —— 按月的新建趋势折线，带最值标记和平均线
- `index.html` —— 上面三个的入口导航页

底层是 go-echarts，生成的是自带 ECharts 的完整 HTML，双击就能看，不需要起服务。

**AI 总结**输出 `<输出目录>/summary.md`：上半部分是模型的分析结论，下半部分是一张所有 Issue 的索引表格，编号带原文链接。它的做法很直接 —— 把所有 Issue 的编号、标题、状态、创建时间、标签拼成一张 Markdown 表格，再加上每条 Issue 的正文，一次性投给模型。

这里有个实用技巧：`aiBaseURL` + `aiModel` + `aiToken` 三个参数是分开的，底层走的是 OpenAI 兼容协议，所以**不限于 DeepSeek**。换成任何 OpenAI 兼容的服务（包括本地跑的 vLLM / Ollama）都可以：

```bash
./issue2file -config=./config.cnf owner/repo \
  -ai -aiModel deepseek-chat -aiBaseURL https://api.deepseek.com/v1
```

> ⚠️ 注意 `-aiBaseURL` 的取值规则，这是实测出来的：工具最终请求的 URL 是 **`aiBaseURL` 直接拼接上 `/chat/completions`**。我用一个本地 mock 服务验证过，填 `http://127.0.0.1:8799/v1` 时收到的请求路径是 `/v1/chat/completions`。所以这里必须填**到版本号为止的根地址**（`https://api.deepseek.com/v1`），不能像仓库示例配置里那样带上 `/chat/completions`，否则拼出来会变成 `.../v1/chat/completions/chat/completions`。

---

## 五、导出结果长什么样

`<输出目录>/` 下的每个 Markdown 文件对应一个 Issue，文件名格式 `issue_<编号>_<标题>.md`（标题做过特殊字符清洗，并截断到 50 个字符）。内容示例：

```markdown
# Issue #1: add github token config file or param

## 基本信息

- **编号**: #1
- **状态**: closed
- **创建者**: @ibarryyan
- **创建时间**: 2025-07-28 01:45:41
- **更新时间**: 2025-09-14 04:08:24
- **关闭时间**: 2025-09-14 04:08:24
- **链接**: https://github.com/ibarryyan/issue2file/issues/1

## 描述

使用环境变量的方式会增加使用者的操作步骤，...
```

开了 `-comment` 的话，正文后面会追加「## 评论」，每条评论标注作者和时间。这套格式对人和对机器都友好 —— 尤其适合直接整目录丢给支持文件投喂的 AI 客户端，或者塞进本地知识库做检索。

---

## 六、6 个实测踩坑点

这一节是上手时真正会卡住你的地方。前 5 条都按当前源码实测过。

**1. 不带 `-config` 会直接 panic**

`-config` 的默认值是 `config.example.conf`，而加载配置的代码要求路径里必须含 `/`。所以这条看着最自然的命令会崩：

```bash
$ ./issue2file owner/repo
panic: config err
```

即使路径不含斜杠也一样：`-config=config.cnf` → 同样 panic。**照着本文第二节那样写成 `-config=./config.cnf` 就行**。

**2. `-config` 不支持绝对路径**

路径拼接会把 `/` 丢掉，`-config=/tmp/foo/config.cnf` 会去找 `/tmp/foo/tmpfoo`。**用相对路径，比如 `-config=./config.cnf`。**

**3. 配置项必须用小驼峰，README 里的下划线写法读不到**

代码读的是 `gitHubToken`、`aiToken`、`aiModel`、`aiBaseURL`、`commentEnable`、`aiEnable`、`chartEnable`、`outputDir`、`summaryFile`。我实测过：写 `github_token = "xxx"` 加载后仍是空值，Token 不会生效。完整正确示例：

```toml
gitHubToken   = "ghp_xxx"      # GitHub Personal Access Token（个人访问令牌）
aiToken       = "sk-xxx"
aiModel       = "deepseek-chat"
aiBaseURL     = "https://api.deepseek.com/v1"   # 注意：不要带 /chat/completions
commentEnable = true
aiEnable      = false
chartEnable   = true
outputDir     = "issues_output"
summaryFile   = "summary.md"
```

**4. 配置文件里的开关会覆盖命令行参数**

代码里字符串配置是「非空才覆盖」，但三个布尔开关是**无条件覆盖**。也就是说配置了 `aiEnable = false` 之后，命令行再写 `-ai` 是没用的。想切换 AI，改配置文件更可靠。

**5. README 里 `--ai true` 这种写法会报错**

Go 的 flag 包会把 `true` 当成一个位置参数，于是工具把它当成仓库地址去解析：

```
level=fatal msg="无法解析仓库地址: 无法解析仓库URL: true"
```

正确写法是 `-ai`（或者 `-ai=true`），同理 `-comment`、`-chart`。另外 README 里提到的 `--ai-summary` 参数在当前代码里并不存在。

**6. 关于 Token 和请求预算**

不配 Token 也能跑，但是匿名访问。GitHub 官方限额是**匿名每小时 60 次请求，带 Personal Access Token 是每小时 5000 次**。开着 `-comment` 时，每条 Issue 会额外发一次评论请求，所以总请求数 ≈ `Issue 数 + Issue 数/100`。粗算：**不配 Token 大概只能导出 50 来条带评论的 Issue**，配了 Token 才能从容处理上千条。

Token 在 GitHub Settings → Developer settings → Personal access tokens 生成，公开仓库勾 `public_repo` 即可，私有仓库需要 `repo` 权限。Token 的读取优先级是**配置文件 `gitHubToken` > 命令行 `-token` > 环境变量 `GITHUB_TOKEN`**（配置文件非空时直接覆盖命令行，这是代码里的实际顺序）。AI 侧同理，`aiToken` / `-aiToken` / `AI_TOKEN` 环境变量三者一致。没有 Token 时不会报错拦截，只是退化成匿名访问并打印一行提示。

补充一个小贴士：Token 写错了不会静默失败，会给出明确报错（我用假 Token 实测）：

```
level=fatal msg="获取issues失败: ... 401 Bad credentials []"
```

---

## 七、什么时候用，什么时候别用

**适合用**：给开源项目做阶段回顾或交接文档；把 Issue 批量整理进本地知识库 / 投喂给 AI；快速看一个陌生仓库的维护负担分布（图表三件套很直观）；定期归档自己仓库的 Issue 快照。

**不适合用**：

- **要增量同步**。它每次都是全量覆盖导出，没有基于更新时间的增量逻辑，也不记录上次抓到哪。仓库很大时，建议先 `outputDir` 分目录，或直接当成一次性快照工具。
- **超大型仓库开 AI 总结**。一次性把所有正文塞进单次请求，Issue 多到一定规模会超出模型上下文窗口；这种情况下建议先按时间/标签分批导出再总结。
- **需要把 PR 和 Issue 严格分开**（见第三节，当前版本会把 PR 一起导出）。
- **要精确过滤**。目前不支持按标签、时间区间或里程碑筛选，拉下来就是全部。

除此之外还有个小规模安全提示：Issue 正文里的 HTML/脚本不会被转义（原样写入 Markdown），如果你打算把导出结果发布到会渲染 HTML 的平台上，先心里有数。

---

## 八、下一步

先把你最常逛的那个仓库导一遍试试，开 `-chart` 看一眼它的状态分布和创建趋势，比翻网页直观得多。

```
github.com/ibarryyan/issue2file
```

项目是 MIT 协议。上手过程中如果遇到本文没覆盖的问题，可以到仓库提 Issue —— 顺带一提，文中提到的几个小坑（`-config` 路径处理、`%!s(...)` 日志打印、PR 过滤）都是几行代码能修的改动，欢迎顺手 PR。

---

*如果这篇对你有用，欢迎点个「在看」或转发给同样在维护仓库的朋友。*
