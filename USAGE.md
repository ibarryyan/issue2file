# Issue2File 使用指南

## 快速开始

### 1. 构建程序
```bash
go build -o issue2file
```

### 2. 基本使用

#### 从当前Git仓库获取Issues
如果你在一个Git仓库目录中：
```bash
./issue2file .
```

#### 从指定仓库获取Issues
```bash
# 使用简短格式
./issue2file owner/repo

# 使用完整URL
./issue2file https://github.com/owner/repo
```

### 3. 设置GitHub Token（推荐）

为了避免API限制，建议设置GitHub Personal Access Token：

```bash
export GITHUB_TOKEN=ghp_your_token_here
./issue2file owner/repo
```

## 输出说明

程序会创建一个名为 `issues_owner_repo` 的目录，包含所有Issue的Markdown文件。

每个文件包含：
- Issue编号和标题
- 状态（open/closed）
- 创建者信息
- 创建和更新时间
- 标签和指派人
- 完整的Issue描述
- GitHub链接

## 示例输出

```
issues_microsoft_calculator/
├── issue_1_Add_scientific_calculator_mode.md
├── issue_2_Bug_division_by_zero.md
└── issue_3_Feature_request_history.md
```

## 常见问题

### Q: API限制怎么办？
A: 设置GITHUB_TOKEN环境变量，可以大大提高API限制。

### Q: 如何获取GitHub Token？
A: 
1. 登录GitHub
2. 进入 Settings > Developer settings > Personal access tokens
3. 生成新token，至少需要 `public_repo` 权限

### Q: 支持私有仓库吗？
A: 支持，但需要设置有相应权限的GitHub Token。

### Q: 程序运行很慢？
A: 大型仓库的Issue数量可能很多，请耐心等待。程序会显示进度。