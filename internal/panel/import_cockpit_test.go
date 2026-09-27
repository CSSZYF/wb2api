package panel

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// cockpitJSON 构造 cockpit tools 导出格式的账号数组（expires_at 为**毫秒**）。
func cockpitJSON(entries ...map[string]any) string {
	b, _ := json.Marshal(entries)
	return string(b)
}

func cockpitEntry(uid, at, rt, domain string) map[string]any {
	return map[string]any{
		"id": "id-" + uid, "email": uid + "@example.com", "uid": uid,
		"nickname": "昵称" + uid, "access_token": at, "refresh_token": rt,
		"expires_at": int64(1893456000000), // 2030-01-01（毫秒）
		"domain":     domain,
	}
}

// newImportPanel 造面板：假上游只服务导入后的签到/余额收尾（失败仅记日志，不阻断）。
func newImportPanel(t *testing.T) (*Panel, string) {
	t.Helper()
	authDir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"remain":100,"total":100}}`))
	}))
	t.Cleanup(srv.Close)
	up := upstream.New()
	up.ChatBaseCN, up.BillingBaseCN, up.WebBaseCN = srv.URL, srv.URL, srv.URL
	up.ChatBaseGlobal, up.BillingBaseGlobal = srv.URL, srv.URL
	p := pool.New("")
	return New(Config{Version: "test", APIKey: "test-key", Pool: p, Upstream: up, AuthDir: authDir}), authDir
}

// postImport 以 multipart 提交 JSON 到 /panel/api/import/cockpit。
func postImport(t *testing.T, pn *Panel, payload string) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "cockpit.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	mw.Close()
	req := httptest.NewRequest("POST", "/panel/api/import/cockpit", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	return rec.Code, got
}

// TestImportCockpitHappyPath 吸收上游 9371f7d：Add Account 对话框支持 cockpit tools
// JSON 导入。导入后账号必须①进池 ②落盘（auths 目录，重启后仍在）③realm 按 domain
// 推断（workbuddy.ai → global）。
func TestImportCockpitHappyPath(t *testing.T) {
	pn, authDir := newImportPanel(t)
	payload := cockpitJSON(
		cockpitEntry("cnuid001", "at-cn", "rt-cn", "www.codebuddy.cn"),
		cockpitEntry("gluid002", "at-gl", "rt-gl", "www.workbuddy.ai"),
	)
	code, got := postImport(t, pn, payload)
	if code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%v", code, got)
	}
	if n, _ := got["imported"].(float64); n != 2 {
		t.Fatalf("imported=%v want 2（%v）", got["imported"], got["errors"])
	}
	if n, _ := got["total"].(float64); n != 2 {
		t.Errorf("total=%v want 2", got["total"])
	}
	// ① 进池。
	if pn.cfg.Pool.AuthByUID("cnuid001") == nil {
		t.Error("cnuid001 未进池")
	}
	gl := pn.cfg.Pool.AuthByUID("gluid002")
	if gl == nil {
		t.Fatal("gluid002 未进池")
	}
	// ③ realm 推断：workbuddy.ai → global。
	if !gl.IsGlobal() {
		t.Errorf("global 账号 realm=%q（应按 domain 推断为 global）", gl.Realm())
	}
	if cn := pn.cfg.Pool.AuthByUID("cnuid001"); cn.IsGlobal() {
		t.Errorf("CN 账号被误判成 global（realm=%q）", cn.Realm())
	}
	// ② 落盘：auths 目录下必须有文件（重启后由 LoadDir 复活）。
	entries, err := os.ReadDir(authDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("auths 目录文件数=%d want 2（未落盘则重启后账号丢失）", len(entries))
	}
	// 落盘内容必须带 accessToken/uid（SaveAtomic 契约）。
	raw, err := os.ReadFile(filepath.Join(authDir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	for _, must := range []string{"accessToken", "uid"} {
		if !bytes.Contains(raw, []byte(must)) {
			t.Errorf("落盘文件缺 %s：%s", must, raw)
		}
	}
}

// TestImportCockpitSkipsInvalidEntries 缺字段/非法 uid 的条目必须进 skipped + errors
// （不静默丢弃，也不因单条坏数据整批失败）：uid 会拼进文件名，非法值（路径字符）
// 必须被拒。
func TestImportCockpitSkipsInvalidEntries(t *testing.T) {
	pn, authDir := newImportPanel(t)
	payload := cockpitJSON(
		cockpitEntry("gooduid", "at", "rt", "www.codebuddy.cn"),
		map[string]any{"id": "no-token", "uid": "u2", "refresh_token": "rt"},                           // 缺 access_token
		map[string]any{"id": "no-uid", "access_token": "at", "refresh_token": "rt"},                    // 缺 uid
		map[string]any{"id": "bad-uid", "uid": "../evil", "access_token": "at", "refresh_token": "rt"}, // 路径字符
	)
	code, got := postImport(t, pn, payload)
	if code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%v", code, got)
	}
	if n, _ := got["imported"].(float64); n != 1 {
		t.Errorf("imported=%v want 1（只有 gooduid 合法）", got["imported"])
	}
	if n, _ := got["skipped"].(float64); n != 3 {
		t.Errorf("skipped=%v want 3", got["skipped"])
	}
	errs, _ := got["errors"].([]any)
	if len(errs) != 3 {
		t.Errorf("errors 应逐条说明跳过原因（便于排查）: %v", got["errors"])
	}
	// 非法 uid 不得落盘（路径穿越防护）。
	entries, _ := os.ReadDir(authDir)
	if len(entries) != 1 {
		t.Errorf("auths 目录文件数=%d want 1（非法 uid 不得写盘）", len(entries))
	}
	for _, e := range entries {
		if e.Name() != "workbuddy-gooduid.json" {
			t.Errorf("意外落盘文件 %q（命名契约 workbuddy-<uid>.json）", e.Name())
		}
	}
}

// TestImportCockpitRejectsBadRequests 非 multipart / 非法 JSON / 空数组 必须 400
// （而不是静默成功），否则前端显示「导入完成：成功 0 个」而用户不知道文件坏了。
func TestImportCockpitRejectsBadRequests(t *testing.T) {
	pn, _ := newImportPanel(t)

	// 非法 JSON。
	if code, got := postImport(t, pn, `{not json`); code != http.StatusBadRequest {
		t.Errorf("非法 JSON: code=%d want 400 body=%v", code, got)
	}
	// 空数组。
	if code, got := postImport(t, pn, `[]`); code != http.StatusBadRequest {
		t.Errorf("空数组: code=%d want 400 body=%v", code, got)
	}
	// 缺 file 字段。
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.Close()
	req := httptest.NewRequest("POST", "/panel/api/import/cockpit", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("缺 file: code=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
}

// TestImportCockpitRequiresAuth 导入是写操作（落盘 + 进池），必须走 withAuth。
func TestImportCockpitRequiresAuth(t *testing.T) {
	pn, _ := newImportPanel(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "x.json")
	_, _ = fw.Write([]byte(cockpitJSON(cockpitEntry("u1", "at", "rt", "www.codebuddy.cn"))))
	mw.Close()
	req := httptest.NewRequest("POST", "/panel/api/import/cockpit", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("无密钥不应放行导入（写操作）")
	}
}

// TestImportCockpitExpiresAtMillis cockpit 的 expires_at 是**毫秒**，落盘前必须转秒：
// 直接照写会把过期时间推到公元 60000 年（NeedsRefresh 永假 → token 永不刷新）。
func TestImportCockpitExpiresAtMillis(t *testing.T) {
	pn, _ := newImportPanel(t)
	if code, got := postImport(t, pn, cockpitJSON(cockpitEntry("msuid", "at", "rt", "www.codebuddy.cn"))); code != http.StatusOK {
		t.Fatalf("code=%d body=%v", code, got)
	}
	a := pn.cfg.Pool.AuthByUID("msuid")
	if a == nil {
		t.Fatal("未进池")
	}
	// 2030-01-01 的秒级时间戳约 1.89e9；毫秒级约 1.89e12。
	if a.ExpiresAt > 4102444800 { // 2100-01-01
		t.Errorf("ExpiresAt=%d 未从毫秒转秒（会永不刷新 token）", a.ExpiresAt)
	}
	if a.ExpiresAt < 1000000000 {
		t.Errorf("ExpiresAt=%d 太小（转换错误）", a.ExpiresAt)
	}
}

// TestImportCockpitFrontendWiring 前端接线：Add Account 对话框必须有「导入 JSON」
// 标签页 + file input + 上传到该端点（缺一处用户就点不到这个功能）。
func TestImportCockpitFrontendWiring(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(js)
	for _, must := range []string{
		`import/cockpit`,  // 端点（multipart，故走原生 fetch 而非 api()）
		`$('importFile')`, // file input
		`'importDone'`,    // 成功回执
		`'importErr'`,     // 失败回执
		`switchAddTab(`,   // 标签页切换
		`'addTabImport'`,  // 导入面板
	} {
		if !strings.Contains(s, must) {
			t.Errorf("app.js 缺 cockpit 导入接线：%s", must)
		}
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	h := string(html)
	for _, must := range []string{`id="importFile"`, `id="importDone"`, `id="importErr"`, `id="addTabs"`, `id="addTabLogin"`, `id="addTabImport"`} {
		if !strings.Contains(h, must) {
			t.Errorf("index.html 缺 cockpit 导入 DOM：%s", must)
		}
	}
	// 文件选择必须是 .json（导入的是 cockpit 导出的 JSON）。
	if !strings.Contains(h, `accept=".json"`) {
		t.Error("index.html 的文件选择未限定 .json")
	}
}
