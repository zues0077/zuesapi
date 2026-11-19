// Package app 管理面板会话鉴权：cookie 会话 + 管理员密码。
//
// 设计（见 AGENTS.md R4 的修订）：
//   - config.admin_password 为空 = 面板不鉴权（仅允许监听环回地址时为空）；
//   - 监听非环回地址时强制要求该字段（Save 层 + 启动层双重把关）；
//   - 会话 token 为随机串，服务端只存哈希；会话表落盘（panel-sessions.json），
//     进程重启后自动恢复，不必重新登录。改密码 / 退出登录会立即作废并回写。
package app

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"workbuddy-api/internal/config"
)

const (
	// sessionCookie 管理面板会话 cookie 名。
	sessionCookie = "ww_admin"
	// sessionTTL 会话有效期；面板每次成功请求续期（滑动过期）。
	// 30 天：面板装在自家 LAN，动辄几个月才看一次统计，7 天太短会频繁要求重登。
	sessionTTL = 30 * 24 * time.Hour
	// sessionFile 会话表落盘文件名（放在 data 目录，与 ledger 同级）。
	// 只存 token 的 SHA-256 与过期时间，文件被读到也无法反推 cookie 原文。
	sessionFile = "panel-sessions.json"
	// sessionSaveGap 滑动续期时的最小落盘间隔：续期只改内存 exp，
	// 频繁写盘没有意义（每 2s 就有一次轮询请求），超过该间隔才落一次。
	sessionSaveGap = 10 * time.Minute
	// sessionSweep 过期会话清理间隔（仅在有请求时顺带触发）。
	sessionSweep = time.Hour
	// loginMaxFail 连续失败次数达到上限后临时拒绝该来源 IP 的登录尝试。
	loginMaxFail = 5
	// loginFailWindow 失败计数窗口 / 锁定时长。
	loginFailWindow = 5 * time.Minute
)

// panelSession 一条已签发的会话：map 以 token 的 SHA-256（hex）为键，只存哈希与过期时间。
type panelSession struct {
	exp   time.Time
	label string // 密码指纹：改密码后旧会话自然失配
}

// loginAttempt 单个来源 IP 的登录失败计数。
type loginAttempt struct {
	fails   int
	firstAt time.Time
	until   time.Time // 非零且在未来 = 锁定中
}

// authState 面板鉴权状态（进程内存 + 可选的落盘副本；Cookie 只存 token 本身）。
type authState struct {
	mu       sync.Mutex
	sessions map[string]*panelSession // tokenHash → session
	attempts map[string]*loginAttempt // 客户端 IP → 登录失败计数
	sweptAt  time.Time
	path     string    // 会话表落盘路径；空 = 不持久化（测试与内存模式）
	lastSave time.Time // 上次落盘时间（续期节流用）
}

// sessionDisk 落盘格式：tokenHash → {exp, label}。
type sessionDisk struct {
	Sessions map[string]diskSession `json:"sessions"`
}

type diskSession struct {
	Exp   int64  `json:"exp"`   // unix 秒
	Label string `json:"label"` // 口令指纹：改密码后旧条目自然失配
}

func newAuthState() *authState {
	return &authState{sessions: map[string]*panelSession{}, attempts: map[string]*loginAttempt{}}
}

// bind 绑定落盘路径并立即恢复上次的会话（进程重启不踢人）。
func (s *authState) bind(path string) {
	s.mu.Lock()
	s.path = path
	s.mu.Unlock()
	s.restore()
}

// restore 从磁盘载入未过期会话。文件缺失/损坏一律忽略（最坏情况只是要求重登）。
func (s *authState) restore() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		return
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var d sessionDisk
	if err := json.Unmarshal(raw, &d); err != nil {
		log.Printf("面板会话表解析失败（忽略，需重新登录）: %v", err)
		return
	}
	now := time.Now()
	restored := 0
	for k, v := range d.Sessions {
		if v.Exp <= 0 || now.After(time.Unix(v.Exp, 0)) {
			continue // 已过期：不再恢复
		}
		s.sessions[k] = &panelSession{exp: time.Unix(v.Exp, 0), label: v.Label}
		restored++
	}
	s.lastSave = time.Now()
	if restored > 0 {
		log.Printf("面板会话已恢复 %d 条（重启前登录态继续有效）", restored)
	}
}

// saveLocked 把当前会话表写盘（原子替换）。调用方需持锁。
// 写盘失败只记日志：会话仍在本进程内存中有效，不影响本次运行。
func (s *authState) saveLocked() {
	if s.path == "" {
		return
	}
	d := sessionDisk{Sessions: make(map[string]diskSession, len(s.sessions))}
	for k, v := range s.sessions {
		d.Sessions[k] = diskSession{Exp: v.exp.Unix(), Label: v.label}
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("面板会话表写入失败: %v", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		log.Printf("面板会话表替换失败: %v", err)
		return
	}
	s.lastSave = time.Now()
}

// panelAuthEnabled 当前是否启用面板鉴权（密码非空）。
func (a *App) panelAuthEnabled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.TrimSpace(a.cfg.AdminPass) != ""
}

// requireRemoteAuth 判断某个监听地址是否必须配置管理员密码：
// 非环回监听（0.0.0.0 / 具体网卡 IP / 空主机名）一律强制。
func requireRemoteAuth(l config.Listen) bool { return !l.IsLoopback() }

// panelLogin 校验管理员密码并签发会话。
// 返回的 brief 为「口令指纹」：前端只持有它判断登录态，无需读取 HttpOnly cookie。
func (a *App) panelLogin(pass, clientIP string) (token, brief string, ok bool, msg string) {
	valid := a.adminPass()
	if strings.TrimSpace(valid) == "" {
		return "", "", true, "未启用管理密码"
	}
	if !a.auth.allowLogin(clientIP) {
		return "", "", false, "尝试次数过多，请 5 分钟后再试"
	}
	if subtle.ConstantTimeCompare([]byte(pass), []byte(valid)) != 1 {
		a.auth.noteFail(clientIP)
		log.Printf("管理面板登录失败 ip=%s", clientIP)
		return "", "", false, "密码错误"
	}
	a.auth.noteSuccess(clientIP)
	brief = passBrief(valid)
	token = a.auth.issue(brief)
	log.Printf("管理面板登录成功 ip=%s", clientIP)
	return token, brief, true, ""
}

// panelLogout 注销当前会话。
func (a *App) panelLogout(r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.auth.revoke(c.Value)
	}
}

// adminPass 读当前管理员密码快照。
func (a *App) adminPass() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.AdminPass
}

// passBrief 口令指纹：改密码后旧 cookie 立即失效（prefix = SHA-256(密码) 前 8 字节的 hex）。
func passBrief(pass string) string {
	sum := sha256.Sum256([]byte("wildwork-admin:" + pass))
	return hex.EncodeToString(sum[:8])
}

// newSessionToken 生成 32 字节随机 token（hex 编码）。
func newSessionToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil { // crypto/rand 失败极罕见：宁可直接失败也不签弱 token
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(buf)
}

// issue 签发会话并返回 token。
func (s *authState) issue(brief string) string {
	tok := newSessionToken()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	s.sessions[tokHash(tok)] = &panelSession{exp: time.Now().Add(sessionTTL), label: brief}
	s.saveLocked() // 登录即刻落盘：紧接着重启也不会被踢下线
	return tok
}

// valid 校验 token 并续期（滚动过期）。token 原文只存在于客户端 cookie。
func (s *authState) valid(tok, brief string) bool {
	if tok == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	key := tokHash(tok)
	sess := s.sessions[key]
	if sess == nil || sess.label != brief {
		return false
	}
	if time.Now().After(sess.exp) {
		delete(s.sessions, key)
		return false
	}
	sess.exp = time.Now().Add(sessionTTL) // 滑动续期：常用面板不会中途掉线
	if time.Since(s.lastSave) > sessionSaveGap {
		s.saveLocked() // 节流落盘：续期本身不需每次都写盘
	}
	return true
}

// revoke 注销单个会话。
func (s *authState) revoke(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := tokHash(tok)
	if _, ok := s.sessions[key]; !ok {
		return
	}
	delete(s.sessions, key)
	s.saveLocked()
}

// tokHash 会话 token → 存储键（SHA-256 hex）。
func tokHash(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// dropAll 清空所有会话（改密码时调用）。
func (s *authState) dropAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = map[string]*panelSession{}
	s.saveLocked() // 同步清空磁盘副本，否则重启后旧会话会被恢复
}

// sweepLocked 顺带清理过期会话（最多每小时一次）。调用方需持锁。
func (s *authState) sweepLocked() {
	now := time.Now()
	if now.Sub(s.sweptAt) < sessionSweep {
		return
	}
	s.sweptAt = now
	removed := false
	for k, v := range s.sessions {
		if now.After(v.exp) {
			delete(s.sessions, k)
			removed = true
		}
	}
	if removed {
		s.saveLocked() // 过期条目同步从磁盘抹掉，避免文件无限增长
	}
	for k, v := range s.attempts {
		if now.After(v.until) && now.Sub(v.firstAt) > loginFailWindow {
			delete(s.attempts, k)
		}
	}
}

// allowLogin 该来源 IP 是否允许尝试登录（失败过多则临时锁定）。
func (s *authState) allowLogin(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	at := s.attempts[ip]
	return at == nil || time.Now().After(at.until)
}

// noteFail 记录一次登录失败；窗口内累计到上限则锁定。
func (s *authState) noteFail(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	at := s.attempts[ip]
	if at == nil || now.Sub(at.firstAt) > loginFailWindow {
		at = &loginAttempt{firstAt: now}
		s.attempts[ip] = at
	}
	at.fails++
	if at.fails >= loginMaxFail {
		at.until = now.Add(loginFailWindow)
		at.fails = 0
		at.firstAt = now
	}
}

// noteSuccess 登录成功后清空失败计数。
func (s *authState) noteSuccess(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.attempts, ip)
}
