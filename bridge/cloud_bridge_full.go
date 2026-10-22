// cloud_bridge.go — QwenPaw 云端实例本地代理桥（Go 版）
//
// 功能：
//   - 本地 HTTP 代理，默认监听 :59000
//   - 转发请求到云端实例 API：base_url = https://<app_id>.qwenpaw.platform.agentscope.io
//     （app_id 来自 oauth JSON 文件）
//   - 认证：Authorization: Bearer <access_token>；收到 401 时用 refresh_token
//     调用平台 /api/v1/auth/refresh 刷新（refresh_token 会轮换，刷新后必须保存），
//     随后原请求重试一次
//   - 转发时剥掉 X-Agent-Id 头的 "cloud-" 前缀（cloud-/cloud2-/... 通用），
//     路径中 /agents/cloud- 前缀段同样剥离（兼容原 Python cloud_bridge.py v15+）
//   - JSON 响应原样透传；GET 200 响应做简单 LRU 内存缓存（默认上限 200MB、TTL 30s）
//   - 额外端点：/__bridge_status（uptime/mem/账号列表/缓存统计）、/__instances（实例列表）
//
// 用法：cloud_bridge_linux [-listen :59000] [-oauth tools/cloud_oauth.json] [-oauth-dir tools]
package main

import (
	"bytes"
	"container/list"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	version         = "go-v1.0"
	platformBase    = "https://platform.agentscope.io"
	refreshEndpoint = "https://platform.agentscope.io/api/v1/auth/refresh"
	baseHostSuffix  = ".qwenpaw.platform.agentscope.io"
	cacheTTL        = 30 * time.Second
	cacheMaxBytes   = int64(200 * 1024 * 1024) // 200MB
)

var hopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailers", "Transfer-Encoding", "Upgrade",
}

// ---------- LRU 缓存（线程安全，按总字节数淘汰） ----------

type cacheEntry struct {
	key     string
	body    []byte
	header  http.Header
	size    int64
	expires time.Time
}

type lruCache struct {
	mu     sync.Mutex
	cap    int64
	ttl    time.Duration
	ll     *list.List
	items  map[string]*list.Element
	size   int64
	hits   int64
	misses int64
}

func newLRUCache(cap int64, ttl time.Duration) *lruCache {
	return &lruCache{cap: cap, ttl: ttl, ll: list.New(), items: make(map[string]*list.Element)}
}

func (c *lruCache) Get(key string) ([]byte, http.Header, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		c.misses++
		return nil, nil, false
	}
	e := el.Value.(*cacheEntry)
	if time.Now().After(e.expires) {
		c.removeLocked(el)
		c.misses++
		return nil, nil, false
	}
	c.ll.MoveToFront(el)
	c.hits++
	return e.body, e.header.Clone(), true
}

func (c *lruCache) Add(key string, body []byte, header http.Header, ttl time.Duration) {
	if len(body) == 0 {
		return
	}
	if ttl <= 0 {
		ttl = c.ttl
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok { // 已有则替换并移到最前
		e := el.Value.(*cacheEntry)
		c.size -= e.size
		e.body = body
		e.header = header.Clone()
		e.size = int64(len(body))
		e.expires = time.Now().Add(ttl)
		c.size += e.size
		c.ll.MoveToFront(el)
	} else {
		e := &cacheEntry{key: key, body: body, header: header.Clone(), size: int64(len(body)), expires: time.Now().Add(ttl)}
		el := c.ll.PushFront(e)
		c.items[key] = el
		c.size += e.size
	}
	for c.size > c.cap && c.ll.Len() > 1 {
		back := c.ll.Back()
		if back == c.ll.Front() {
			break
		}
		c.removeLocked(back)
	}
}

// InvalidatePrefix 删除 key 以 prefix 开头的缓存条目（写操作后失效对应会话缓存）。
func (c *lruCache) InvalidatePrefix(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, el := range c.items {
		if strings.HasPrefix(k, prefix) {
			e := el.Value.(*cacheEntry)
			c.ll.Remove(el)
			delete(c.items, k)
			c.size -= e.size
		}
	}
}

func (c *lruCache) removeLocked(el *list.Element) {
	e := el.Value.(*cacheEntry)
	c.ll.Remove(el)
	delete(c.items, e.key)
	c.size -= e.size
}

func (c *lruCache) stats() (size int64, entries int, hits, misses int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.size, len(c.items), c.hits, c.misses
}

// ---------- Bridge ----------

type accountInfo struct {
	OAuthFile      string  `json:"oauth_file"`
	AppID          string  `json:"app_id"`
	BaseURL        string  `json:"base_url"`
	Status         string  `json:"status"`
	TokenRemainMin float64 `json:"token_remain_min"`
}

type Bridge struct {
	oauthPath string
	oauthDir  string
	client    *http.Client
	cache     *lruCache
	start     time.Time

	mu       sync.Mutex // 保护 oauth map 与 baseURL
	oauth    map[string]interface{}
	baseURL  string
	oauthErr string
}

func NewBridge(oauthPath, oauthDir string) *Bridge {
	b := &Bridge{
		oauthPath: oauthPath,
		oauthDir:  oauthDir,
		client: &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConnsPerHost:   16,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 120 * time.Second,
			},
		},
		cache: newLRUCache(cacheMaxBytes, cacheTTL),
		start: time.Now(),
	}
	b.loadOAuth()
	go b.tokenPrefetch()
	return b
}

// tokenPrefetch 后台预刷新：token 剩余 <30min 时主动 refresh（避免请求时才发现 401/403）
func (b *Bridge) tokenPrefetch() {
	lastRefresh := time.Now().Add(-time.Minute)
	for {
		time.Sleep(60 * time.Second)
		at := b.accessToken()
		if at == "" {
			continue
		}
		if jwtRemainMin(at) < 30 && time.Since(lastRefresh) > time.Minute {
			if err := b.refreshToken(); err != nil {
				log.Printf("[auth] prefetch refresh failed: %v", err)
			} else {
				log.Printf("[auth] prefetch refreshed token（剩余<30min 主动续期）")
				lastRefresh = time.Now()
			}
		}
	}
}

func (b *Bridge) loadOAuth() {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, err := os.ReadFile(b.oauthPath)
	if err != nil {
		b.oauth = nil
		b.baseURL = ""
		b.oauthErr = fmt.Sprintf("oauth file not readable: %v", err)
		b.fallbackOAuthLocked() // 主 oauth 文件不存在 → 尝试切到 oauthDir 里其他有实例账号
		return
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		b.oauth = nil
		b.baseURL = ""
		b.oauthErr = fmt.Sprintf("oauth parse error: %v", err)
		b.fallbackOAuthLocked() // 解析失败 → 同样 fallback
		return
	}
	b.oauth = m
	appID, _ := firstString(m, "app_id", "appId")
	if appID == "" {
		b.oauthErr = "oauth file missing app_id"
		b.fallbackOAuthLocked() // 主 oauth 无实例（app_id 空）→ fallback 到有实例账号
		return
	}
	b.baseURL = "https://" + appID + baseHostSuffix
	b.oauthErr = ""
}

// fallbackOAuthLocked 主 oauth 不可用（文件不存在/解析失败/缺 app_id）时，
// 从 oauthDir 中找第一个有 app_id 的账号作为主账号，避免代理转发到空 base_url。
// 调用方必须持有 b.mu。
func (b *Bridge) fallbackOAuthLocked() {
	if b.oauthDir == "" {
		return
	}
	entries, err := os.ReadDir(b.oauthDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "cloud_oauth") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		fp := filepath.Join(b.oauthDir, e.Name())
		if fp == b.oauthPath {
			continue
		}
		d2, err2 := os.ReadFile(fp)
		if err2 != nil {
			continue
		}
		var m2 map[string]interface{}
		if json.Unmarshal(d2, &m2) != nil {
			continue
		}
		a2, _ := firstString(m2, "app_id", "appId")
		if a2 == "" {
			continue
		}
		b.oauthPath = fp
		b.oauth = m2
		b.baseURL = "https://" + a2 + baseHostSuffix
		b.oauthErr = ""
		log.Printf("[auth] 主 oauth 不可用 -> fallback 到 %s（base=%s）", filepath.Base(fp), b.baseURL)
		return
	}
	b.baseURL = ""
}

func (b *Bridge) accessToken() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.oauth == nil {
		return ""
	}
	t, _ := firstString(b.oauth, "access_token", "accessToken")
	return t
}

// refreshToken 用平台 auth/refresh 刷新 access_token / refresh_token（会轮换），写回 oauth 文件。
func (b *Bridge) refreshToken() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.oauth == nil {
		return fmt.Errorf("no oauth config loaded")
	}
	rt, _ := firstString(b.oauth, "refresh_token", "refreshToken")
	if rt == "" {
		return fmt.Errorf("oauth file missing refresh_token")
	}
	body, _ := json.Marshal(map[string]string{"refreshToken": rt})
	req, err := http.NewRequest(http.MethodPost, refreshEndpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("refresh request failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("refresh failed status=%d body=%s", resp.StatusCode, truncate(string(respBody), 200))
	}
	var m map[string]interface{}
	if err := json.Unmarshal(respBody, &m); err != nil {
		return fmt.Errorf("refresh response parse error: %w", err)
	}
	newAT, _ := firstString(m, "accessToken", "access_token")
	newRT, _ := firstString(m, "refreshToken", "refresh_token")
	if newAT == "" || newRT == "" {
		if data, ok := m["data"].(map[string]interface{}); ok {
			newAT, _ = firstString(data, "accessToken", "access_token")
			newRT, _ = firstString(data, "refreshToken", "refresh_token")
		}
	}
	if newAT == "" || newRT == "" {
		return fmt.Errorf("refresh response missing tokens: %s", truncate(string(respBody), 200))
	}
	b.oauth["access_token"] = newAT
	b.oauth["refresh_token"] = newRT
	out, _ := json.MarshalIndent(b.oauth, "", "  ")
	if err := os.WriteFile(b.oauthPath, out, 0o644); err != nil {
		return fmt.Errorf("failed to persist refreshed oauth: %w", err)
	}
	log.Printf("[auth] token refreshed via %s (new refresh_token saved)", refreshEndpoint)
	return nil
}

// jwtRemainMin 解码 JWT payload 的 exp，返回剩余分钟（-1 表示无效/已过期）
func jwtRemainMin(tok string) float64 {
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return -1
	}
	payload := parts[1]
	if pad := 4 - len(payload)%4; pad < 4 {
		payload += strings.Repeat("=", pad)
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return -1
	}
	var m struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &m) != nil || m.Exp == 0 {
		return -1
	}
	remain := m.Exp - time.Now().Unix()
	if remain < 0 {
		return -1
	}
	return float64(remain) / 60
}

// loadOAuthFile 读取指定 oauth JSON 文件（多账号路由用）。
func (b *Bridge) loadOAuthFile(fp string) (map[string]interface{}, bool) {
	data, err := os.ReadFile(fp)
	if err != nil {
		return nil, false
	}
	var m map[string]interface{}
	if json.Unmarshal(data, &m) != nil {
		return nil, false
	}
	return m, true
}

// refreshOAuthFile 用指定 oauth 的 refresh_token 刷新并写回该文件（多账号 401 自愈）。
func (b *Bridge) refreshOAuthFile(om map[string]interface{}, fp string) bool {
	rt, _ := firstString(om, "refresh_token", "refreshToken")
	if rt == "" {
		return false
	}
	body, _ := json.Marshal(map[string]string{"refreshToken": rt})
	req, err := http.NewRequest(http.MethodPost, refreshEndpoint, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var m map[string]interface{}
	if json.Unmarshal(respBody, &m) != nil {
		return false
	}
	newAT, _ := firstString(m, "accessToken", "access_token")
	newRT, _ := firstString(m, "refreshToken", "refresh_token")
	if newAT == "" || newRT == "" {
		if data, ok := m["data"].(map[string]interface{}); ok {
			newAT, _ = firstString(data, "accessToken", "access_token")
			newRT, _ = firstString(data, "refreshToken", "refresh_token")
		}
	}
	if newAT == "" || newRT == "" {
		return false
	}
	om["access_token"] = newAT
	om["refresh_token"] = newRT
	out, _ := json.MarshalIndent(om, "", "  ")
	if os.WriteFile(fp, out, 0o644) != nil {
		return false
	}
	log.Printf("[auth] 已刷新账号 oauth %s", filepath.Base(fp))
	return true
}

// scanAccounts 列出 oauth-dir 下所有 cloud_oauth*.json 账号（不输出 token）。
func (b *Bridge) scanAccounts() []accountInfo {
	accounts := []accountInfo{}
	if b.oauthDir == "" {
		return accounts
	}
	matches, _ := filepath.Glob(filepath.Join(b.oauthDir, "cloud_oauth*.json"))
	for _, p := range matches {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var m map[string]interface{}
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		appID, _ := firstString(m, "app_id", "appId")
		at, _ := firstString(m, "access_token", "accessToken")
		status := "no-token"
		if at != "" {
			if jwtRemainMin(at) < 0 {
				status = "invalid-token"
			} else {
				status = "running"
			}
		}
		info := accountInfo{OAuthFile: filepath.Base(p), AppID: appID, Status: status, TokenRemainMin: -1}
		if at != "" {
			info.TokenRemainMin = jwtRemainMin(at)
		}
		if appID != "" {
			info.BaseURL = "https://" + appID + baseHostSuffix
		}
		accounts = append(accounts, info)
	}
	return accounts
}

func (b *Bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Agent-Id, X-Account, Accept")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch r.URL.Path {
	case "/__bridge_status":
		b.handleStatus(w, r)
		return
	case "/__instances":
		b.handleInstances(w, r)
		return
	case "/__import/login":
		b.handleImportLogin(w, r)
		return
	case "/__import/remove":
		b.handleImportRemove(w, r)
		return
	case "/__panel":
		b.handlePanel(w, r)
		return
	case "/__instance/start", "/__instance/stop", "/__instance/restart":
		b.handleInstanceAction(w, r)
		return
	}
	b.proxy(w, r)
}

func (b *Bridge) handleStatus(w http.ResponseWriter, r *http.Request) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	b.mu.Lock()
	base := b.baseURL
	oauthErr := b.oauthErr
	b.mu.Unlock()
	cacheSize, cacheEntries, hits, misses := b.cache.stats()
	at := b.accessToken()
	tokenOK := at != ""
	remainMin := -1.0
	if tokenOK {
		remainMin = jwtRemainMin(at)
		if remainMin < 0 {
			tokenOK = false
		}
	}
	writeJSON(w, map[string]interface{}{
		"version":          version,
		"uptime_s":         int64(time.Since(b.start).Seconds()),
		"mem_mb":           float64(ms.Alloc) / 1024 / 1024,
		"listen":           r.Host,
		"base_url":         base,
		"oauth_file":       b.oauthPath,
		"token_ok":         tokenOK,
		"token_remain_min": remainMin,
		"oauth_error":      oauthErr,
		"accounts":         b.scanAccounts(),
		"cache": map[string]interface{}{
			"max_bytes":  cacheMaxBytes,
			"size_bytes": cacheSize,
			"entries":    cacheEntries,
			"hits":       hits,
			"misses":     misses,
		},
	})
}

func (b *Bridge) handleInstances(w http.ResponseWriter, r *http.Request) {
	insts := b.scanAccounts()
	b.probeMissingAppIDs(insts) // 空 app_id 账号自动探测实例并写回 oauth（用户开通后刷新即显示）
	writeJSON(w, map[string]interface{}{"instances": insts})
}

// accountPrefixMap 返回有实例账号的前缀映射（按 oauth 文件名排序）：
// acct1- → 第一个有 app_id 的账号，acct2- → 第二个……（QwenPaw 主界面聚合智能体用）
func (b *Bridge) accountPrefixMap() (map[string]string, []string) {
	m := map[string]string{}
	var order []string
	insts := b.scanAccounts()
	idx := 1
	for _, inst := range insts {
		if inst.AppID == "" {
			continue
		}
		prefix := fmt.Sprintf("acct%d", idx)
		m[prefix] = inst.OAuthFile
		order = append(order, prefix)
		idx++
	}
	return m, order
}

// agentsAllAccounts 聚合所有有实例账号的智能体（QwenPaw 主界面用）：id 加账号前缀、name 加账号标识。
func (b *Bridge) agentsAllAccounts() ([]map[string]interface{}, error) {
	acctMap, order := b.accountPrefixMap()
	merged := []map[string]interface{}{}
	for _, prefix := range order {
		oauthFile := acctMap[prefix]
		fp := filepath.Join(b.oauthDir, oauthFile)
		om, ok := b.loadOAuthFile(fp)
		if !ok {
			continue
		}
		app, _ := firstString(om, "app_id", "appId")
		at, _ := firstString(om, "access_token", "accessToken")
		if app == "" || at == "" {
			continue
		}
		base := "https://" + app + baseHostSuffix
		req, err := http.NewRequest(http.MethodGet, base+"/api/agents", nil)
		if err != nil {
			continue
		}
		req.Header.Set("Authorization", "Bearer "+at)
		req.Header.Set("X-Agent-Id", "default")
		resp, err := b.client.Do(req)
		if err != nil {
			continue
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			continue
		}
		var v map[string]interface{}
		if json.Unmarshal(respBody, &v) != nil {
			continue
		}
		ags, _ := v["agents"].([]interface{})
		acctName := strings.TrimPrefix(oauthFile, "cloud_oauth_")
		acctName = strings.TrimSuffix(acctName, ".json")
		for _, ag := range ags {
			if am, ok := ag.(map[string]interface{}); ok {
				aid, _ := am["id"].(string)
				aname, _ := am["name"].(string)
				if aid == "" {
					aid = "default"
				}
				am["id"] = prefix + "-" + aid
				if aname == "" {
					aname = aid
				}
				am["name"] = fmt.Sprintf("[%s] %s", acctName, aname)
				merged = append(merged, am)
			}
		}
	}
	return merged, nil
}
// 解决"平台已开通实例但 oauth 没记录 app_id"的场景——刷新面板即可自动补齐。
func (b *Bridge) probeMissingAppIDs(insts []accountInfo) {
	for i := range insts {
		if insts[i].AppID != "" {
			continue
		}
		fp := filepath.Join(b.oauthDir, insts[i].OAuthFile)
		data, err := os.ReadFile(fp)
		if err != nil {
			continue
		}
		var om map[string]interface{}
		if json.Unmarshal(data, &om) != nil {
			continue
		}
		at, _ := firstString(om, "access_token", "accessToken")
		if at == "" {
			continue
		}
		app := b.probeAppID(at)
		if app == "" {
			continue
		}
		om["app_id"] = app
		out, _ := json.MarshalIndent(om, "", "  ")
		if os.WriteFile(fp, out, 0o644) == nil {
			insts[i].AppID = app
			insts[i].BaseURL = "https://" + app + baseHostSuffix
			log.Printf("[import] 自动探测补齐 app_id for %s -> %s", insts[i].OAuthFile, app)
		}
	}
}

// handleImportLogin 账号密码登录平台，保存 oauth 到 tools/cloud_oauth_<name>.json。
// 登录响应缺 app_id 时，用 Bearer token 探测 GET /api/v1/app/list。
func (b *Bridge) handleImportLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "POST only"})
		return
	}
	var req struct {
		Account  string `json:"account"`
		Username string `json:"username"`
		Password string `json:"password"`
		Name     string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "bad json: " + err.Error()})
		return
	}
	// 兼容前端自动重登（username 字段）和手动登录（account 字段）
	acct := req.Account
	if acct == "" {
		acct = req.Username
	}
	if acct == "" || req.Password == "" {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "account/password required"})
		return
	}

	loginBody, _ := json.Marshal(map[string]string{"account": acct, "password": req.Password})
	lreq, err := http.NewRequest(http.MethodPost, platformBase+"/api/v1/auth/login", bytes.NewReader(loginBody))
	if err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	lreq.Header.Set("Content-Type", "application/json")
	lresp, err := b.client.Do(lreq)
	if err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "login request failed: " + err.Error()})
		return
	}
	defer lresp.Body.Close()
	lbody, _ := io.ReadAll(io.LimitReader(lresp.Body, 1<<20))
	if lresp.StatusCode != http.StatusOK {
		writeJSON(w, map[string]interface{}{"ok": false, "error": fmt.Sprintf("login failed status=%d body=%s", lresp.StatusCode, truncate(string(lbody), 200))})
		return
	}
	var m map[string]interface{}
	if err := json.Unmarshal(lbody, &m); err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "login response parse: " + err.Error()})
		return
	}
	at, _ := firstString(m, "accessToken", "access_token")
	rt, _ := firstString(m, "refreshToken", "refresh_token")
	if at == "" || rt == "" {
		// 平台登录响应 token 可能在 data 嵌套里
		if data, ok := m["data"].(map[string]interface{}); ok {
			at, _ = firstString(data, "accessToken", "access_token")
			rt, _ = firstString(data, "refreshToken", "refresh_token")
		}
	}
	if at == "" || rt == "" {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "login response missing tokens: " + truncate(string(lbody), 200)})
		return
	}
	appID, _ := firstString(m, "app_id", "appId")
	if appID == "" {
		appID = b.probeAppID(at) // 从 app/list 探测实例 id
	}
	name := sanitizeName(req.Name)
	// acctLocal：去掉邮箱 @ 后缀（如 2817894809@qq.com -> 2817894809），兼容旧 oauth / 默认名
	acctLocal := acct
	if i := strings.IndexByte(acctLocal, '@'); i >= 0 {
		acctLocal = acctLocal[:i]
	}
	// 用户没填账号名 → 默认用账号 @ 前部分（避免多个账号都叫 "1" 互相覆盖）
	if req.Name == "" && (name == "" || name == "1") {
		alt := sanitizeName(acctLocal)
		if alt != "" && alt != "1" {
			name = alt
		}
	}
	oauth := map[string]interface{}{
		"access_token":  at,
		"refresh_token": rt,
		"app_id":        appID,
		"name":          name,
		"account":       acct, // 登录账号标识（邮箱/手机号），用于重复检测
	}
	targetPath := filepath.Join(b.oauthDir, "cloud_oauth_"+name+".json")
	duplicate := false
	dupFile := ""
	conflict := false // 同名但确认为不同账号 → 不覆盖，给新账号改名
	if b.oauthDir != "" {
		entries, err := os.ReadDir(b.oauthDir)
		if err == nil {
			for _, e := range entries {
				if e.IsDir() || !strings.HasPrefix(e.Name(), "cloud_oauth") || !strings.HasSuffix(e.Name(), ".json") {
					continue
				}
				fp := filepath.Join(b.oauthDir, e.Name())
				d, err2 := os.ReadFile(fp)
				if err2 != nil {
					continue
				}
				var om map[string]interface{}
				if json.Unmarshal(d, &om) != nil {
					continue
				}
				oa, _ := firstString(om, "account")
				on, _ := firstString(om, "name")
				sameAcct := (oa != "" && oa == acct) || (on != "" && (on == acct || on == acctLocal))
				if sameAcct {
					// 同一云端账号 → 覆盖更新（合并），不新建
					targetPath = fp
					duplicate = true
					dupFile = e.Name()
					conflict = false
					break
				}
				// 同名：旧 oauth 无 account（历史数据）→ 保守覆盖；有明确不同 account → 冲突改名，绝不覆盖别的账号
				if on != "" && on == name {
					if oa == "" {
						targetPath = fp
						duplicate = true
						dupFile = e.Name()
						conflict = false
						break
					}
					conflict = true
				}
			}
		}
	}
	// 同名但不同账号 → 新账号自动改名（如 xxx_2），保留原账号数据
	if conflict && !duplicate {
		base := name
		for i := 2; i < 50; i++ {
			cand := fmt.Sprintf("%s_%d", base, i)
			if _, err := os.Stat(filepath.Join(b.oauthDir, "cloud_oauth_"+cand+".json")); os.IsNotExist(err) {
				name = cand
				oauth["name"] = cand
				break
			}
		}
		targetPath = filepath.Join(b.oauthDir, "cloud_oauth_"+name+".json")
	}
	out, _ := json.MarshalIndent(oauth, "", "  ")
	if err := os.WriteFile(targetPath, out, 0o644); err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "save oauth failed: " + err.Error()})
		return
	}
	log.Printf("[import] saved oauth for %s -> %s (app_id=%s dup=%v)", name, targetPath, appID, duplicate)
	resp := map[string]interface{}{"ok": true, "name": name, "app_id": appID, "oauth_file": filepath.Base(targetPath)}
	if duplicate {
		resp["duplicate"] = true
		resp["message"] = "账号已存在，已更新（未新建）"
		resp["merged_into"] = dupFile
	}
	writeJSON(w, resp)
}

// probeAppID 用 access token 探测 GET /api/v1/app/list 里的 qwenpaw 实例 app_id。
func (b *Bridge) probeAppID(accessToken string) string {
	req, err := http.NewRequest(http.MethodGet, platformBase+"/api/v1/app/list", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := b.client.Do(req)
	if err != nil {
		log.Printf("[import] app/list request failed: %v", err)
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		log.Printf("[import] app/list status=%d body=%s", resp.StatusCode, truncate(string(body), 200))
		return ""
	}
	var v interface{}
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	id := findAppID(v)
	if id == "" {
		log.Printf("[import] app/list no app_id found: %s", truncate(string(body), 200))
	}
	return id
}

// handleImportRemove 删除一个账号（删除 data/ 下对应 oauth 文件；删除主账号后自动重载/fallback）。
func (b *Bridge) handleImportRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "POST only"})
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "bad json: " + err.Error()})
		return
	}
	if req.Name == "" {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "name required"})
		return
	}
	// 兼容输入：cloud_oauth_xxx.json / cloud_oauth_xxx / xxx.json / xxx
	base := req.Name
	if !strings.HasPrefix(base, "cloud_oauth_") {
		base = "cloud_oauth_" + base
	}
	if !strings.HasSuffix(base, ".json") {
		base = base + ".json"
	}
	// 安全：拒绝路径穿越
	if strings.ContainsAny(base, `/\`) {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "bad name"})
		return
	}
	fp := filepath.Join(b.oauthDir, base)
	if err := os.Remove(fp); err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "account not found: " + base})
		} else {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "remove failed: " + err.Error()})
		}
		return
	}
	log.Printf("[import] removed oauth %s", fp)
	// 删除主 oauth 后重新加载（loadOAuth 会自动 fallback 到其他有实例账号）
	b.loadOAuth()
	writeJSON(w, map[string]interface{}{"ok": true, "message": "已删除 " + base, "oauth_file": base})
}

// handlePanel 返回内联 HTML 账号管理面板。
func (b *Bridge) handlePanel(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, panelHTML)
}

// handleInstanceAction 简化实现：stop 忽略；start/restart 返回 ok（更新/重启由外部编排）。
func (b *Bridge) handleInstanceAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "POST only"})
		return
	}
	action := strings.TrimPrefix(r.URL.Path, "/__instance/")
	var req struct {
		Name string `json:"name"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)
	log.Printf("[instance] %s name=%s (simplified)", action, req.Name)
	writeJSON(w, map[string]interface{}{"ok": true, "action": action, "name": req.Name})
}

func (b *Bridge) proxy(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	base := b.baseURL
	oauthErr := b.oauthErr
	b.mu.Unlock()
	if base == "" {
		http.Error(w, "cloud bridge not configured: "+oauthErr, http.StatusInternalServerError)
		return
	}

	// 按账号路由：前端带 X-Account（oauth 文件名）时，用该账号的实例/凭证转发（多账号会话各自独立）
	var acct map[string]interface{}
	acctPath := ""
	if acctName := r.Header.Get("X-Account"); acctName != "" {
		acctPath = filepath.Join(b.oauthDir, acctName)
		if om, ok := b.loadOAuthFile(acctPath); ok {
			acct = om
			if app, _ := firstString(om, "app_id", "appId"); app != "" {
				base = "https://" + app + baseHostSuffix
				oauthErr = ""
			}
		}
	} else {
		// QwenPaw 主界面：X-Agent-Id 带账号前缀（acct1-/acct2-…）时按前缀路由到对应账号
		if xa := r.Header.Get("X-Agent-Id"); xa != "" {
			acctMap, _ := b.accountPrefixMap()
			for prefix, oauthFile := range acctMap {
				if strings.HasPrefix(xa, prefix+"-") {
					acctPath = filepath.Join(b.oauthDir, oauthFile)
					if om, ok := b.loadOAuthFile(acctPath); ok {
						acct = om
						if app, _ := firstString(om, "app_id", "appId"); app != "" {
							base = "https://" + app + baseHostSuffix
							oauthErr = ""
						}
					}
					break
				}
			}
		}
	}

	// 读取并缓存请求体（401 重试需要重放）
	var reqBody []byte
	if r.Body != nil {
		reqBody, _ = io.ReadAll(io.LimitReader(r.Body, 256<<20)) // 上限 256MB
		r.Body.Close()
	}

	// 转发目标：base_url + 原路径（剥 cloud- 前缀）+ query
	targetPath := stripCloudPrefixPath(r.URL.Path)
	// 兼容 QwenPaw 前端非 /api 路径（云端对未知路径返回 SPA HTML，必须映射到 /api）
	if targetPath == "/agents" {
		targetPath = "/api/agents"
	}
	// 兼容云端无 messages 端点：历史消息在 chats/{id} 详情里（同 Python 桥）
	if strings.HasPrefix(targetPath, "/api/chats/") && strings.HasSuffix(targetPath, "/messages") {
		targetPath = strings.TrimSuffix(targetPath, "/messages")
	}
	target := base + targetPath
	if r.URL.RawQuery != "" {
		target += "?" + stripCloudPrefixQuery(r.URL.RawQuery)
	}

	// 聚合智能体：无账号路由且请求 /api/agents → 返回所有账号的智能体（QwenPaw 主界面多账号展示）
	if acct == nil && (targetPath == "/api/agents" || targetPath == "/agents") {
		if ags, err := b.agentsAllAccounts(); err == nil {
			writeJSON(w, map[string]interface{}{"agents": ags})
			return
		}
	}

	// 实时聊天是流式（HTTP/2 stream）：必须边收边发，不能 io.ReadAll 缓冲（否则前端拿不到实时片段、一直转圈）
	if r.Method == http.MethodPost && (strings.HasPrefix(targetPath, "/api/console/chat") || strings.Contains(targetPath, "/stream")) {
		b.proxyStream(w, r, target, reqBody, true, acct, acctPath)
		return
	}

	start := time.Now()
	status, respHeader, respBody, err := b.forward(r.Context(), r.Method, target, reqBody, r.Header, true, 0, acct, acctPath)
	dur := time.Since(start).Milliseconds()
	if err != nil {
		log.Printf("[proxy] %s %s -> ERR (%dms): %v", r.Method, targetPath, dur, err)
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	log.Printf("[proxy] %s %s -> %d (%dms, %dB)", r.Method, targetPath, status, dur, len(respBody))
	log.Printf("[req-dbg] raw=%s mapped=%s xagent=%q origin=%q referer=%q", r.URL.Path, targetPath, r.Header.Get("X-Agent-Id"), r.Header.Get("Origin"), r.Header.Get("Referer"))
	if status == http.StatusForbidden || status == http.StatusUnauthorized {
		log.Printf("[auth-dbg] %s %s -> %d reqXAgentId=%q reqHeaders=%v respBody=%s",
			r.Method, targetPath, status, r.Header.Get("X-Agent-Id"),
			r.Header.Get("Origin"), truncate(string(respBody), 300))
	}

	for _, h := range hopHeaders {
		respHeader.Del(h)
	}
	respHeader.Del("Content-Length")
	respHeader.Del("Access-Control-Allow-Origin")
	respHeader.Del("Access-Control-Allow-Methods")
	respHeader.Del("Access-Control-Allow-Headers")
	// CORS 头统一由 ServeHTTP 开头设置（避免重复头）
	for k, vs := range respHeader {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(status)
	if len(respBody) > 0 {
		w.Write(respBody)
	}
}

// proxyStream 流式转发（边收边发）：用于 /api/console/chat 实时聊天等 HTTP/2 stream/SSE 端点。
// 与 forward 不同：不用 io.ReadAll 等全量，读一块写一块 + Flush，前端能实时收到文字。
// acct/acctPath：按账号路由（X-Account 指定时非空）——用该账号的 access_token、401 刷新该账号。
func (b *Bridge) proxyStream(w http.ResponseWriter, r *http.Request, target string, body []byte, allowRetry bool, acct map[string]interface{}, acctPath string) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "stream request error: "+err.Error(), http.StatusBadGateway)
		return
	}
	for k, vs := range r.Header {
		if isHopHeader(k) || strings.EqualFold(k, "Host") {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if v := req.Header.Get("X-Agent-Id"); v != "" {
		// 先剥账号前缀（acct1-/acct2-…），再剥 cloud- 前缀
		if i := strings.Index(v, "-"); i > 0 && strings.HasPrefix(v[:i], "acct") {
			v = v[i+1:]
		}
		req.Header.Set("X-Agent-Id", stripCloudPrefix(v))
	}
	req.Header.Del("Origin")
	req.Header.Del("Referer")
	// token：按账号路由时用该账号 access_token；否则用主 oauth token
	token := b.accessToken()
	if acct != nil {
		if t, _ := firstString(acct, "access_token", "accessToken"); t != "" {
			token = t
		}
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := b.client.Do(req)
	if err != nil {
		log.Printf("[proxy] STREAM %s -> ERR: %v", target, err)
		http.Error(w, "upstream stream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// 401/403 → 刷新 token 后重试一次（流式重放；按账号时刷新该账号）
	if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && allowRetry {
		resp.Body.Close()
		ok := false
		if acct != nil && acctPath != "" {
			ok = b.refreshOAuthFile(acct, acctPath)
		} else {
			ok = b.refreshToken() == nil
		}
		if ok {
			b.proxyStream(w, r, target, body, false, acct, acctPath)
			return
		}
		http.Error(w, "unauthorized", resp.StatusCode)
		return
	}

	for _, h := range hopHeaders {
		resp.Header.Del(h)
	}
	resp.Header.Del("Content-Length")
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	log.Printf("[proxy] STREAM %s -> %d (streaming)", target, resp.StatusCode)

	// 边收边发：读一块写一块 + Flush（前端实时看到生成文字）
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // 客户端断开
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			return
		}
	}
}

// forward 发送一次转发；401 且 allowRetry 时刷新 token 后重试一次。
// acct/acctPath：按账号路由（X-Account 指定时非空）——用该账号的 access_token 转发、401 刷新该账号。
func (b *Bridge) forward(ctx context.Context, method, target string, body []byte, srcHeader http.Header, allowRetry bool, retried int, acct map[string]interface{}, acctPath string) (int, http.Header, []byte, error) {
	b.mu.Lock()
	base := b.baseURL
	b.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	for k, vs := range srcHeader {
		if isHopHeader(k) || strings.EqualFold(k, "Host") {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	// X-Agent-Id：先剥账号前缀（acct1-/acct2-…，QwenPaw 主界面聚合智能体路由），再剥 cloud- 前缀
	if v := req.Header.Get("X-Agent-Id"); v != "" {
		if i := strings.Index(v, "-"); i > 0 && strings.HasPrefix(v[:i], "acct") {
			v = v[i+1:]
		}
		req.Header.Set("X-Agent-Id", stripCloudPrefix(v))
	}
	// 剥掉浏览器 Origin/Referer（云端网关对非云端 Origin 返回 403——这是智能体加载不出来的根因）
	req.Header.Del("Origin")
	req.Header.Del("Referer")
	// token：按账号路由时用该账号 access_token；否则用主 oauth token
	token := b.accessToken()
	if acct != nil {
		if t, _ := firstString(acct, "access_token", "accessToken"); t != "" {
			token = t
		}
	}
	req.Header.Set("Authorization", "Bearer "+token)

	cacheKey := ""
	// POST/写操作：失效对应会话详情缓存（历史消息缓存 24h，写完要能立刻看到新内容）
	if method == http.MethodPost {
		if strings.HasPrefix(target, base+"/api/chats/") {
			b.cache.InvalidatePrefix(target)
		} else if strings.HasPrefix(target, base+"/api/chats") {
			b.cache.InvalidatePrefix(base + "/api/chats")
		}
	}
	if method == http.MethodGet {
		cacheKey = target
		// 缓存 key 必须含 X-Agent-Id——不同智能体同路径请求会串缓存（历史对话串号根因）
		if xa := req.Header.Get("X-Agent-Id"); xa != "" {
			cacheKey += "|agent=" + xa
		}
		if cached, hdr, ok := b.cache.Get(cacheKey); ok {
			return http.StatusOK, hdr, cached, nil
		}
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 512<<20))
	if err != nil {
		return resp.StatusCode, resp.Header.Clone(), respBody, err
	}
	hdr := resp.Header.Clone()

	if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && allowRetry && retried == 0 {
		log.Printf("[auth] %d on %s, refreshing token and retrying once", resp.StatusCode, target)
		ok := false
		if acct != nil && acctPath != "" {
			ok = b.refreshOAuthFile(acct, acctPath)
		} else {
			ok = b.refreshToken() == nil
		}
		if ok {
			return b.forward(ctx, method, target, body, srcHeader, false, retried+1, acct, acctPath)
		}
		log.Printf("[auth] refresh failed for %s", target)
	}

	if resp.StatusCode == http.StatusOK && method == http.MethodGet && cacheKey != "" {
		// 会话详情/历史消息基本不变 → 缓存 24h（避免每次请求云端 15-20s）；其余 30s
		ttl := cacheTTL
		if strings.HasPrefix(target, base+"/api/chats/") {
			ttl = 24 * time.Hour
		}
		b.cache.Add(cacheKey, respBody, hdr, ttl)
	}
	return resp.StatusCode, hdr, respBody, nil
}

// ---------- 工具函数 ----------

// parentAlive 检测指定 PID 的进程是否存活（桥跟随父进程退出用）。
func parentAlive(pid int) bool {
	if pid <= 0 {
		return true
	}
	if runtime.GOOS == "windows" {
		out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH").Output()
		if err != nil {
			return true // 无法查询时保守认为存活
		}
		return strings.Contains(string(out), fmt.Sprintf("%d", pid))
	}
	// Linux/macOS：/proc/<pid> 目录存在即存活（零外部依赖、零 syscall）
	_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	return err == nil
}

func sanitizeName(s string) string {
	var b strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			b.WriteRune(c)
		}
	}
	if b.Len() == 0 {
		return "1"
	}
	return b.String()
}

// findAppID 递归在平台 app/list 响应中找 qwenpaw 实例 app_id（优先 app_id/appId 字段，退而求其次找 UUID 形状的 id）。
func findAppID(v interface{}) string {
	switch t := v.(type) {
	case map[string]interface{}:
		for _, k := range []string{"app_id", "appId"} {
			if s, ok := t[k].(string); ok && s != "" {
				return s
			}
		}
		for _, k := range []string{"id"} {
			if s, ok := t[k].(string); ok && looksLikeUUID(s) {
				return s
			}
		}
		for _, val := range t {
			if s := findAppID(val); s != "" {
				return s
			}
		}
	case []interface{}:
		for _, val := range t {
			if s := findAppID(val); s != "" {
				return s
			}
		}
	}
	return ""
}

func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func stripCloudPrefix(s string) string {
	if !strings.HasPrefix(s, "cloud") {
		return s
	}
	if i := strings.Index(s, "-"); i >= 0 {
		return s[i+1:]
	}
	return s
}

func stripCloudPrefixPath(p string) string {
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		if strings.HasPrefix(seg, "cloud") && strings.Contains(seg, "-") {
			segs[i] = stripCloudPrefix(seg)
		}
	}
	return strings.Join(segs, "/")
}

func stripCloudPrefixQuery(q string) string {
	vals, err := url.ParseQuery(q)
	if err != nil {
		return q
	}
	changed := false
	for _, key := range []string{"agent_id", "agentId"} {
		if vs, ok := vals[key]; ok {
			for i, v := range vs {
				vs[i] = stripCloudPrefix(v)
			}
			changed = true
		}
	}
	if !changed {
		return q
	}
	return vals.Encode()
}

func isHopHeader(k string) bool {
	for _, h := range hopHeaders {
		if strings.EqualFold(h, k) {
			return true
		}
	}
	return false
}

func firstString(m map[string]interface{}, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s, true
			}
		}
	}
	return "", false
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

const panelHTML = `<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>☁ QwenPaw 云端账号管理</title>
<style>
body{font-family:system-ui,-apple-system,sans-serif;background:#f5f7fa;margin:0;padding:24px;color:#222}
.card{background:#fff;border-radius:12px;box-shadow:0 1px 4px rgba(0,0,0,.08);padding:20px;margin-bottom:16px;max-width:720px}
h2{margin:0 0 4px}h3{margin:16px 0 8px;color:#333}
.badge{display:inline-block;padding:2px 10px;border-radius:10px;font-size:12px;background:#e8f5e9;color:#2e7d32;margin-left:8px}
.badge.bad{background:#fdecea;color:#c62828}
table{width:100%;border-collapse:collapse;font-size:14px}
th,td{text-align:left;padding:8px 6px;border-bottom:1px solid #eee;word-break:break-all}
input{width:100%;padding:8px;margin:4px 0 10px;border:1px solid #ccc;border-radius:6px;box-sizing:border-box}
button{background:#1a73e8;color:#fff;border:0;border-radius:6px;padding:8px 14px;cursor:pointer;margin:2px}
button.small{padding:4px 10px;font-size:12px;background:#eef1f5;color:#333}
button.green{background:#0a8043}
pre{background:#f1f3f5;padding:10px;border-radius:8px;font-size:12px;overflow-x:auto}
#msg{margin-top:8px;font-size:13px;color:#c62828}
</style>
</head>
<body>
<div class="card">
<h2>☁ QwenPaw 云端账号管理 <span id="bridgebadge" class="badge">…</span></h2>
<p style="color:#666;font-size:13px">桥状态 / token 有效性 / 内存 / 缓存命中</p>
<pre id="status"></pre>
</div>
<div class="card">
<h3>账号 / 实例列表</h3>
<table id="instances"><tr><th>oauth 文件</th><th>app_id</th><th>base_url</th></tr></table>
</div>
<div class="card">
<h3>登录新平台账号</h3>
<form id="login">
<label>账号</label><input id="account" placeholder="platform.agentscope.io 账号">
<label>密码</label><input id="password" type="password" placeholder="密码">
<label>名称（保存为 cloud_oauth_&lt;名称&gt;.json）</label><input id="name" placeholder="如：主账号">
<button type="submit" class="green">登录并保存</button>
</form>
<div id="msg"></div>
</div>
<div class="card">
<h3>实例操作（简化：stop 忽略，start/restart 返回 ok）</h3>
<button class="small" data-action="start" data-name="">start</button>
<button class="small" data-action="stop" data-name="">stop</button>
<button class="small" data-action="restart" data-name="">restart</button>
<span style="color:#999;font-size:12px">需配合账号 name（见下方账号列表文件名）</span>
</div>
<script>
function $(id){return document.getElementById(id)}
function refresh(){
  fetch('/__bridge_status').then(r=>r.json()).then(s=>{
    $('status').textContent = JSON.stringify({version:s.version,uptime_s:s.uptime_s,mem_mb:s.mem_mb.toFixed(1),token_ok:s.token_ok,base_url:s.base_url,oauth_error:s.oauth_error,cache_hits:s.cache.hits,cache_misses:s.cache.misses},null,2);
    var b=$('bridgebadge'); b.textContent=s.token_ok?'桥在线 token OK':'桥在线 token 缺失'; b.className='badge'+(s.token_ok?'':' bad');
  }).catch(e=>{$('status').textContent='无法连接桥: '+e});
  fetch('/__instances').then(r=>r.json()).then(s=>{
    var t=$('instances'); t.innerHTML='<tr><th>oauth 文件</th><th>app_id</th><th>base_url</th></tr>';
    (s.instances||[]).forEach(i=>{
      var tr=document.createElement('tr');
      tr.innerHTML='<td>'+i.oauth_file+'</td><td>'+(i.app_id||'-')+'</td><td>'+(i.base_url||'-')+'</td>';
      t.appendChild(tr);
    });
  }).catch(()=>{});
}
$('login').addEventListener('submit',function(e){
  e.preventDefault();
  var body={account:$('account').value,password:$('password').value,name:$('name').value};
  fetch('/__import/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)})
    .then(r=>r.json()).then(j=>{
      $('msg').textContent = j.ok?('✅ 已保存账号 '+j.name+(j.app_id?' (app_id='+j.app_id+')':'')):('❌ '+j.error);
      if(j.ok){refresh();$('name').value='';$('password').value='';}
    }).catch(e=>{$('msg').textContent='请求失败: '+e});
});
document.querySelectorAll('[data-action]').forEach(function(btn){
  btn.addEventListener('click',function(){
    var name=this.dataset.name||'';
    var action=this.dataset.action;
    fetch('/__instance/'+action,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({name:name})})
      .then(r=>r.json()).then(j=>{$('msg').textContent=JSON.stringify(j)})
      .catch(e=>{$('msg').textContent='请求失败: '+e});
  });
});
refresh();setInterval(refresh,15000);
</script>
</body>
</html>`

func main() {
	listen := flag.String("listen", ":59000", "local listen address")
	oauthPath := flag.String("oauth", "tools/cloud_oauth.json", "oauth JSON file with access_token/refresh_token/app_id")
	oauthDir := flag.String("oauth-dir", "tools", "directory to scan for cloud_oauth*.json (accounts list)")
	flag.Parse()

	b := NewBridge(*oauthPath, *oauthDir)
	// 跟随父进程退出：父进程（QwenPaw backend/启动器）消失后桥自动退出（15 秒检测），
	// 解决 QwenPaw 强杀/崩溃时钩子不执行导致的桥孤儿残留——自退出无需任何权限。
	parentPid := os.Getppid()
	go func() {
		for {
			time.Sleep(15 * time.Second)
			if !parentAlive(parentPid) {
				log.Printf("[lifecycle] 父进程 %d 已退出，桥跟随退出", parentPid)
				os.Exit(0)
			}
		}
	}()
	srv := &http.Server{Addr: *listen, Handler: b}

	log.Printf("cloud_bridge %s listening on %s (oauth=%s)", version, *listen, *oauthPath)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}