// profile.go Web 控制台账号资料（上游 f1496d0 / issue #94：改名后免重登同步昵称）。
//
// GET {webBase}/console/account，Bearer + x-client-platform: web（与 tasks/claim
// 同一鉴权形态，实测 2026-10-01：无需 Web 会话 cookie）。
//
// 隐私边界（重要）：该接口响应包含手机号（phoneNumber）等个人敏感信息。本方法
// 只解析 nickname 与 uid（uid 仅做一致性核对），其余字段一概不解析、不落日志、
// 不透传——调用方也拿不到。响应体读入内存后即被丢弃（只留两个字符串），
// 敏感字段在**解析层**就被丢掉，不进入任何后续路径。昵称同步只在面板手动「刷新」
// 时触发，后台余额定时刷新不调用本接口。
//
// 与上游实现的差异（本仓修正）：上游 profile.go 把 Origin/Referer 硬编码为 CN
// （https://www.workbuddy.cn），global 账号会头域不一致（请求打到 workbuddy.ai
// 却声明 CN Origin）。本仓按 realm 取——webBase(a) 已按 realm 切域，Origin/Referer
// 与之同域，与 headers.go 的 BillingHeaders / tasks.go 的 claimViaWeb 口径一致。
// ⚠️ 该端点在 global 域是否同形**未验证**：调用方（panel.syncNicknames）对单号
// 失败（含路径不存在）一律静默跳过，不写脏、不报错。
package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// accountProfilePath Web 控制台账号资料路径（实测）。
const accountProfilePath = "/console/account"

// FetchAccountProfile 拉取账号资料并返回最新昵称。uid 与凭证不一致时报错
// （防串号）；业务/网络错误（含 404 路径不存在）原样返回，调用方静默跳过即可。
func (c *Client) FetchAccountProfile(a *auth.Auth) (string, error) {
	base := c.webBase(a)
	req, err := http.NewRequest(http.MethodGet, base+accountProfilePath, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("x-client-platform", "web")
	// Origin/Referer 按 realm 取（= 请求同域），不硬编码 CN：global 号打到
	// workbuddy.ai 时头域必须一致，否则上游可能按头域不符判非法请求。
	req.Header.Set("Origin", base)
	req.Header.Set("Referer", base+"/profile/account-settings")
	if ua := c.userAgent(a); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	data, err := c.doJSON(req)
	if err != nil {
		return "", err
	}
	// 只取两个字段：敏感信息（手机号等）在这里就被丢弃，不进入任何后续路径。
	var resp struct {
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("profile parse: %w", err)
	}
	if resp.UID != "" && a.UID != "" && resp.UID != a.UID {
		return "", fmt.Errorf("profile uid mismatch: resp=%s auth=%s", resp.UID, a.UID)
	}
	return resp.Nickname, nil
}
