package main

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// 从.git/config文件中解析仓库信息
func getRepoFromGitConfig() (owner, repo string, err error) {
	configPath := ".git/config"
	
	file, err := os.Open(configPath)
	if err != nil {
		return "", "", fmt.Errorf("无法打开.git/config文件: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	
	// 查找origin远程仓库的URL
	var inOriginSection bool
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		
		// 检查是否进入origin section
		if strings.Contains(line, `[remote "origin"]`) {
			inOriginSection = true
			continue
		}
		
		// 如果进入了新的section，退出origin section
		if inOriginSection && strings.HasPrefix(line, "[") && !strings.Contains(line, `[remote "origin"]`) {
			inOriginSection = false
			continue
		}
		
		// 在origin section中查找url
		if inOriginSection && strings.HasPrefix(line, "url = ") {
			url := strings.TrimPrefix(line, "url = ")
			return parseRepoURL(url)
		}
	}

	if err := scanner.Err(); err != nil {
		return "", "", fmt.Errorf("读取.git/config文件失败: %w", err)
	}

	return "", "", fmt.Errorf("在.git/config中未找到origin远程仓库")
}

// 解析各种格式的仓库URL
func parseRepoURL(repoURL string) (owner, repo string, err error) {
	// 去除前后空格
	repoURL = strings.TrimSpace(repoURL)
	
	// 处理不同格式的URL
	patterns := []struct {
		regex *regexp.Regexp
		desc  string
	}{
		// HTTPS格式: https://github.com/owner/repo.git
		{regexp.MustCompile(`^https://github\.com/([^/]+)/([^/]+?)(?:\.git)?/?$`), "HTTPS URL"},
		// SSH格式: git@github.com:owner/repo.git
		{regexp.MustCompile(`^git@github\.com:([^/]+)/([^/]+?)(?:\.git)?/?$`), "SSH URL"},
		// 简短格式: owner/repo
		{regexp.MustCompile(`^([^/]+)/([^/]+)$`), "简短格式"},
	}

	for _, pattern := range patterns {
		if matches := pattern.regex.FindStringSubmatch(repoURL); len(matches) == 3 {
			return matches[1], matches[2], nil
		}
	}

	return "", "", fmt.Errorf("无法解析仓库URL: %s", repoURL)
}