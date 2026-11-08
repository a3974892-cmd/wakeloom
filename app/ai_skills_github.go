package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

var aiGitHubComponent = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func aiGitHubSkillURL(raw string) (download, subdir string, err error) {
	u, e := url.Parse(strings.TrimSpace(raw))
	if e != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil {
		return "", "", errors.New("请粘贴 https://github.com/作者/仓库 或技能文件夹链接")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || len(parts) > 30 {
		return "", "", errors.New("GitHub 链接不完整")
	}
	for _, p := range parts {
		if !aiGitHubComponent.MatchString(p) || p == "." || p == ".." {
			return "", "", errors.New("GitHub 链接含不支持的路径，请下载 ZIP 后本地安装")
		}
	}
	repo := strings.TrimSuffix(parts[1], ".git")
	download = "https://api.github.com/repos/" + parts[0] + "/" + repo + "/zipball"
	if len(parts) > 2 {
		if len(parts) < 4 || (parts[2] != "tree" && parts[2] != "blob") {
			return "", "", errors.New("请使用仓库、tree 文件夹或 SKILL.md 的 blob 链接")
		}
		download += "/" + url.PathEscape(parts[3])
		subdir = strings.Join(parts[4:], "/")
		if parts[2] == "blob" {
			if !strings.EqualFold(path.Base(subdir), "SKILL.md") {
				return "", "", errors.New("文件链接须指向 SKILL.md")
			}
			subdir = path.Dir(subdir)
			if subdir == "." {
				subdir = ""
			}
		}
	}
	return download, subdir, nil
}

func aiGitHubRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 || req.URL.Scheme != "https" || req.URL.User != nil || req.URL.Port() != "" || (req.URL.Hostname() != "api.github.com" && req.URL.Hostname() != "codeload.github.com") {
		return errors.New("GitHub 下载重定向地址不受支持")
	}
	return nil
}

func aiSkillFromGitHubZIP(b []byte, subdir string) (*aiSkill, error) {
	p := &aiSkillPackage{}
	if err := p.addZIP(b, true, subdir); err != nil {
		return nil, err
	}
	if err := p.discover(); err != nil {
		return nil, err
	}
	if len(p.Candidates) != 1 {
		return nil, errors.New("此链接包含多个技能，请在新版安装预览里选择，或粘贴具体技能目录链接")
	}
	return p.selectSkill(p.Candidates[0].Path)
}

func aiGitHubSkillClient() *http.Client {
	return &http.Client{Transport: transport, Timeout: 45 * time.Second, CheckRedirect: aiGitHubRedirect}
}

func aiDownloadGitHubSkill(r *http.Request, raw string) (*aiSkill, error) {
	return aiDownloadGitHubSkillWithClient(r, raw, aiGitHubSkillClient())
}

func aiDownloadGitHubSkillWithClient(r *http.Request, raw string, client *http.Client) (*aiSkill, error) {
	b, subdir, err := aiFetchGitHubSkillZIP(r, raw, client)
	if err != nil {
		return nil, err
	}
	return aiSkillFromGitHubZIP(b, subdir)
}

func aiFetchGitHubSkillZIP(r *http.Request, raw string, client *http.Client) ([]byte, string, error) {
	u, subdir, err := aiGitHubSkillURL(raw)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(r.Context(), "GET", u, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "Wakeloom")
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("GitHub 下载失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("GitHub 返回 %d：请检查公开仓库链接、分支名或网络；私有仓库请下载后本地安装", resp.StatusCode)
	}
	b, err := aiReadBounded(resp.Body, aiUploadLimit)
	if err != nil {
		return nil, "", errors.New("仓库下载超过 32 MB，请下载后仅选择技能目录安装")
	}
	return b, subdir, nil
}
