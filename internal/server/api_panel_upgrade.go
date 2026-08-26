package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/zxcll/vps-panel/internal/selfupdate"
	"github.com/zxcll/vps-panel/internal/store"
)

// panelVersion 是面板版本号。放在这里而不是 cmd/panel：
// server 包要拿它和 GitHub 上的最新版比，而 main 包是没法被 import 的。
// cmd/panel 反过来引用 server.PanelVersion()，保证只有一个来源。
const panelVersion = "1.6.0"

// PanelVersion 返回当前面板版本。
func PanelVersion() string { return panelVersion }

// httpClient 是面板对外发请求用的客户端（查更新、下二进制）。
// 带超时：GitHub 偶尔会把连接挂住，没有超时会让升级请求一直卡着。
var httpClient = &http.Client{Timeout: 60 * time.Second}

// 面板自更新。
//
// 和探针升级共用 internal/selfupdate 那套「下载 → 校验能不能跑 → 备份后原子替换」，
// 但**风险高一档**：探针砸了只丢一台机器的监控，面板砸了整个控制面都没了。
// 所以这里多加了一道探针升级没有的保险 —— 用 Release 里的 SHA256SUMS.txt 校验。
// （探针是从面板下的，可信度不一样；面板是从公网下的，得自己验。）
//
// 替换完先把响应发出去、再退出，让 systemd 把新版本拉起来。
// install.sh 生成的单元里有 Restart=always，这是前提。

const (
	// githubRepo 是取版本用的仓库。和 install.sh 里的默认值保持一致。
	githubRepo = "zxcll/vps-panel"
	// releaseCacheTTL 是最新版信息的缓存时长。别每刷一次页面就打一次 GitHub。
	releaseCacheTTL = 10 * time.Minute
	// panelDownloadTimeout 是下载面板二进制的超时。它比探针大（约 13MB），
	// 而且是从公网下的，给宽一点。
	panelDownloadTimeout = 5 * time.Minute
	// minPanelBinarySize 是合理的下限。
	minPanelBinarySize = 4 << 20
	// panelExitDelay 是回复浏览器之后、真正退出之前的等待。
	panelExitDelay = 1500 * time.Millisecond
)

// panelUpgradeTarget 描述怎么替换面板自己的二进制。
var panelUpgradeTarget = selfupdate.Target{
	ExpectOutput: "vps-panel",
	MinSize:      minPanelBinarySize,
	TempPrefix:   ".vps-panel-new-",
}

// releaseInfo 是 GitHub Release 的精简信息。
type releaseInfo struct {
	TagName     string    `json:"tag_name"`
	Version     string    `json:"version"`
	Body        string    `json:"body"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	// assets 只在内部用，不回给前端。
	assets map[string]string
}

// releaseCache 缓存最新版信息。
type releaseCache struct {
	mu        sync.Mutex
	info      *releaseInfo
	fetched   time.Time
	lastErr   string
	upgrading bool
}

// --- 版本查询 ---

func (s *Server) handlePanelVersion(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	current := PanelVersion()

	out := map[string]any{
		"current_version": current,
		"arch":            runtime.GOARCH,
		// BinaryPath 让用户知道要升的是哪个文件，出问题时也知道去哪儿找 .old。
		"binary_path": panelBinaryPath(),
	}

	latest, err := s.latestRelease(ctx, r.URL.Query().Get("refresh") == "1")
	if err != nil {
		out["check_error"] = err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}

	out["latest_version"] = latest.Version
	out["release_notes"] = latest.Body
	out["release_url"] = latest.HTMLURL
	out["published_at"] = latest.PublishedAt
	out["has_update"] = latest.Version != "" && latest.Version != current
	writeJSON(w, http.StatusOK, out)
}

// latestRelease 取 GitHub 上最新的 Release，带缓存。
func (s *Server) latestRelease(ctx context.Context, force bool) (*releaseInfo, error) {
	s.rel.mu.Lock()
	if !force && s.rel.info != nil && time.Since(s.rel.fetched) < releaseCacheTTL {
		info := s.rel.info
		s.rel.mu.Unlock()
		return info, nil
	}
	s.rel.mu.Unlock()

	proxy, err := s.githubProxy(ctx)
	if err != nil {
		return nil, err
	}

	url := proxied(proxy, "https://api.github.com/repos/"+githubRepo+"/releases/latest")
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连不上 GitHub 检查更新：%w"+
			"（国内机器可以在设置里填一个加速前缀）", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GitHub 返回 HTTP %d：%s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var raw struct {
		TagName     string    `json:"tag_name"`
		Body        string    `json:"body"`
		HTMLURL     string    `json:"html_url"`
		PublishedAt time.Time `json:"published_at"`
		Assets      []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("解析 GitHub 响应失败：%w", err)
	}

	info := &releaseInfo{
		TagName:     raw.TagName,
		Version:     strings.TrimPrefix(raw.TagName, "v"),
		Body:        raw.Body,
		HTMLURL:     raw.HTMLURL,
		PublishedAt: raw.PublishedAt,
		assets:      map[string]string{},
	}
	for _, a := range raw.Assets {
		info.assets[a.Name] = a.BrowserDownloadURL
	}

	s.rel.mu.Lock()
	s.rel.info, s.rel.fetched = info, time.Now()
	s.rel.mu.Unlock()
	return info, nil
}

// githubProxy 读用户配的加速前缀。
func (s *Server) githubProxy(ctx context.Context) (string, error) {
	cfg, err := s.st.LoadSettings(ctx)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(cfg.GitHubProxy), nil
}

// proxied 给 URL 套上加速前缀。语义和 install.sh 的 GITHUB_PROXY 一致。
func proxied(proxy, url string) string {
	if proxy == "" {
		return url
	}
	return strings.TrimRight(proxy, "/") + "/" + url
}

// --- 执行升级 ---

func (s *Server) handlePanelUpgrade(w http.ResponseWriter, r *http.Request) {
	// 同一时刻只允许一个升级在跑。两个并发的替换会互相踩临时文件和 .old。
	s.rel.mu.Lock()
	if s.rel.upgrading {
		s.rel.mu.Unlock()
		writeError(w, http.StatusConflict, "已经有一个升级在进行中")
		return
	}
	s.rel.upgrading = true
	s.rel.mu.Unlock()
	defer func() {
		s.rel.mu.Lock()
		s.rel.upgrading = false
		s.rel.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), panelDownloadTimeout+time.Minute)
	defer cancel()

	latest, err := s.latestRelease(ctx, true)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if latest.Version == PanelVersion() {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "replaced": false,
			"message": fmt.Sprintf("已经是最新版 %s，无需升级", PanelVersion()),
		})
		return
	}

	proxy, _ := s.githubProxy(ctx)

	// 先把校验和拉下来。拿不到就直接停手 —— 面板的二进制是从公网下的，
	// 没有校验和就等于裸奔，而装错了整个控制面就没了。
	sums, err := s.fetchChecksums(ctx, latest, proxy)
	if err != nil {
		writeError(w, http.StatusBadGateway,
			"拿不到 Release 的 SHA256SUMS.txt，已放弃升级："+err.Error())
		return
	}

	name := "panel-linux-" + runtime.GOARCH
	data, err := s.downloadAsset(ctx, latest, proxy, name)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	want, ok := sums[name]
	if !ok {
		writeError(w, http.StatusBadGateway,
			fmt.Sprintf("SHA256SUMS.txt 里没有 %s 这一项，已放弃升级", name))
		return
	}
	got := sha256.Sum256(data)
	if hex.EncodeToString(got[:]) != want {
		writeError(w, http.StatusBadGateway,
			fmt.Sprintf("下到的 %s 校验和对不上（期望 %s，实际 %s），已放弃升级",
				name, want[:16]+"…", hex.EncodeToString(got[:])[:16]+"…"))
		return
	}

	out, err := panelUpgradeTarget.Apply(ctx, strings.NewReader(string(data)), s.log)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 面板升了，探针也该跟上 —— 否则「升级探针」会一直说已是最新，很误导。
	agentMsg := s.refreshAgentBinaries(ctx, latest, proxy, sums)

	s.st.AddEvent(ctx, nil, store.EventSystem, store.LevelWarn,
		fmt.Sprintf("面板已升级：%s → %s，即将重启", PanelVersion(), latest.Version))
	s.log.Warn("面板升级完成，即将退出让 systemd 重启",
		"从", PanelVersion(), "到", latest.Version, "二进制", out.BinaryPath)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"replaced":     true,
		"from_version": PanelVersion(),
		"to_version":   latest.Version,
		"binary_path":  out.BinaryPath,
		"backup_path":  out.BackupPath,
		"agents":       agentMsg,
		"message": fmt.Sprintf("已升级到 %s，面板正在重启，大约 5 秒后刷新页面即可。%s",
			latest.Version, agentMsg),
	})

	// 响应先发出去，再退出。反过来的话浏览器只会看到连接断开，
	// 用户根本不知道升成了没有。
	go s.exitForUpgrade()
}

// exitForUpgrade 在响应发出去之后退出进程，让 systemd 用新二进制拉起来。
func (s *Server) exitForUpgrade() {
	time.Sleep(panelExitDelay)
	s.log.Info("面板退出，等待 systemd 用新版本拉起")
	osExit(0)
}

// osExit 做成变量只为让单测能拦住它。
var osExit = os.Exit

// fetchChecksums 拉 Release 里的 SHA256SUMS.txt，解析成 文件名 → 校验和。
func (s *Server) fetchChecksums(ctx context.Context, rel *releaseInfo, proxy string) (map[string]string, error) {
	data, err := s.downloadAsset(ctx, rel, proxy, "SHA256SUMS.txt")
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		// 格式是 "<sha256>  <文件名>"，文件名可能带 * 前缀。
		out[strings.TrimPrefix(fields[1], "*")] = fields[0]
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("SHA256SUMS.txt 内容解析不出任何条目")
	}
	return out, nil
}

// downloadAsset 下载 Release 里的一个文件。
func (s *Server) downloadAsset(ctx context.Context, rel *releaseInfo, proxy, name string) ([]byte, error) {
	url, ok := rel.assets[name]
	if !ok {
		return nil, fmt.Errorf("Release %s 里没有 %s 这个文件", rel.TagName, name)
	}

	dlCtx, cancel := context.WithTimeout(ctx, panelDownloadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, proxied(proxy, url), nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载 %s 失败：%w", name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载 %s 时 GitHub 返回 HTTP %d", name, resp.StatusCode)
	}
	// 限个上限，别让一个畸形响应把内存吃光。面板二进制约 13MB。
	return io.ReadAll(io.LimitReader(resp.Body, 128<<20))
}

// refreshAgentBinaries 把 data/agents/ 下四个架构的探针一起更新。
//
// 失败不影响面板升级本身：探针二进制旧一点只是「升级探针」按钮暂时用不了，
// 而面板已经换好了，不能因为这个把整件事回滚。
func (s *Server) refreshAgentBinaries(ctx context.Context, rel *releaseInfo,
	proxy string, sums map[string]string) string {

	dir := s.cfg.AgentsDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "探针二进制目录建不出来：" + err.Error()
	}

	var ok, failed []string
	for arch := range supportedArch {
		name := "agent-linux-" + arch
		data, err := s.downloadAsset(ctx, rel, proxy, name)
		if err != nil {
			failed = append(failed, arch)
			continue
		}
		if want, has := sums[name]; has {
			got := sha256.Sum256(data)
			if hex.EncodeToString(got[:]) != want {
				failed = append(failed, arch+"(校验和不符)")
				continue
			}
		}
		// 先写临时文件再原子替换：写一半被打断的话，节点会下到半个二进制。
		tmp := filepath.Join(dir, ".new-"+name)
		if err := os.WriteFile(tmp, data, 0o755); err != nil {
			failed = append(failed, arch)
			continue
		}
		if err := os.Rename(tmp, s.agentBinaryPath(arch)); err != nil {
			os.Remove(tmp)
			failed = append(failed, arch)
			continue
		}
		ok = append(ok, arch)
	}

	msg := fmt.Sprintf("探针二进制已更新 %d 个架构", len(ok))
	if len(failed) > 0 {
		msg += "，失败：" + strings.Join(failed, "、")
	}
	return msg
}

// panelBinaryPath 返回面板自己的二进制路径，拿不到时返回空串。
func panelBinaryPath() string {
	p, err := panelUpgradeTarget.Path()
	if err != nil {
		return ""
	}
	return p
}
