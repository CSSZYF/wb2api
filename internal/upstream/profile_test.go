// profile_test.go 钉住账号资料拉取（上游 f1496d0 / issue #94）：Bearer + web 平台头
// 可访问 /console/account；只解析 nickname/uid；uid 不一致防串号；业务错误原样返回。
//
// 与上游的差异（本仓修正）：Origin/Referer 按 realm 取（webBase(a)），不再硬编码 CN
// ——否则 global 号会头域不一致（上游 profile.go 写死 https://www.workbuddy.cn）。
package upstream

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestFetchAccountProfile(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/console/account") {
			return nil, errors.New("wrong path")
		}
		if r.Header.Get("Authorization") == "" || r.Header.Get("x-client-platform") != "web" {
			return nil, errors.New("missing bearer / web platform header")
		}
		// 响应刻意带 phoneNumber：方法必须只解析 nickname/uid，敏感字段不得进入返回值。
		return jsonResp(200, `{"code":0,"msg":"OK","data":{"uid":"u1","nickname":"新名字","phoneNumber":"13800000000"}}`), nil
	})
	c.WebBaseCN = "https://web.example"
	nick, err := c.FetchAccountProfile(&auth.Auth{UID: "u1", AccessToken: "at"})
	if err != nil || nick != "新名字" {
		t.Fatalf("nick=%q err=%v, want 新名字 nil", nick, err)
	}
}

func TestFetchAccountProfileUIDMismatch(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"uid":"someone-else","nickname":"x"}}`), nil
	})
	c.WebBaseCN = "https://web.example"
	if _, err := c.FetchAccountProfile(&auth.Auth{UID: "u1", AccessToken: "at"}); err == nil {
		t.Fatal("uid 不一致应报错（防串号）")
	}
}

func TestFetchAccountProfileBusinessError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(401, `{"code":1002,"msg":"unauthorized"}`), nil
	})
	c.WebBaseCN = "https://web.example"
	if _, err := c.FetchAccountProfile(&auth.Auth{UID: "u1", AccessToken: "bad"}); err == nil {
		t.Fatal("401 应返回错误")
	}
}

// TestFetchAccountProfileNotFound 路径不存在（404）必须返回错误——global 域是否同形
// 未验证，调用方（syncNicknames）据此**静默跳过**该号而不是当作成功。
func TestFetchAccountProfileNotFound(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(404, `not found`), nil
	})
	c.WebBaseCN = "https://web.example"
	if _, err := c.FetchAccountProfile(&auth.Auth{UID: "u1", AccessToken: "at"}); err == nil {
		t.Fatal("404 应返回错误（global 域同形未验证 → 调用方静默跳过）")
	}
}

// TestFetchAccountProfileRealmOrigin CN 用 CN 官网域，global 用国际站域——Origin/
// Referer 与请求 host 三者必须同域，否则 global 号头域不一致（上游硬编码 CN 的缺陷）。
func TestFetchAccountProfileRealmOrigin(t *testing.T) {
	type seen struct{ host, origin, referer string }
	cases := []struct {
		name       string
		global     bool
		wantDomain string
	}{
		{"cn", false, "https://web.example"},
		{"global", true, defaultGlobalBase},
	}
	for _, cse := range cases {
		t.Run(cse.name, func(t *testing.T) {
			var got seen
			c := testClient(func(r *http.Request) (*http.Response, error) {
				got = seen{r.URL.Scheme + "://" + r.URL.Host, r.Header.Get("Origin"), r.Header.Get("Referer")}
				return jsonResp(200, `{"code":0,"data":{"uid":"u1","nickname":"n"}}`), nil
			})
			c.WebBaseCN = "https://web.example"
			c.GlobalEnabled = true
			a := &auth.Auth{UID: "u1", AccessToken: "at"}
			if cse.global {
				if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := c.FetchAccountProfile(a); err != nil {
				t.Fatalf("err=%v", err)
			}
			if got.host != cse.wantDomain {
				t.Errorf("请求 host=%q want %q", got.host, cse.wantDomain)
			}
			if got.origin != cse.wantDomain {
				t.Errorf("Origin=%q want %q（必须按 realm 取，不得硬编码 CN）", got.origin, cse.wantDomain)
			}
			if !strings.HasPrefix(got.referer, cse.wantDomain) {
				t.Errorf("Referer=%q 应与 Origin 同域 %q", got.referer, cse.wantDomain)
			}
		})
	}
}

// TestProfileSourceDropsSensitiveFields 源码级隐私边界：profile.go 里除 nickname/uid
// 外**不得**有任何敏感字段的解析（手机号等）。响应体进了内存的只有两个字符串，
// 敏感字段在解析层就被丢弃——这条边界写死在实现里，本测试把它钉死。
//
// 判据是**解析面**（json struct tag）而非全文：注释里说明「响应含 phoneNumber、
// 一律不解析」正是这条边界的文档，不该被误伤；真正要禁的是把它解析出来。
func TestProfileSourceDropsSensitiveFields(t *testing.T) {
	raw, err := os.ReadFile("profile.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	// 必须只解析这两个字段。
	for _, want := range []string{`json:"nickname"`, `json:"uid"`} {
		if !strings.Contains(src, want) {
			t.Errorf("profile.go 缺字段解析 %s", want)
		}
	}
	// 不得出现任何敏感字段的 json tag（= 不得被解析）。
	for _, banned := range []string{
		`json:"phone`, `json:"Phone`, `json:"mobile`, `json:"Mobile`,
		`json:"email`, `json:"Email`, `json:"realName`, `json:"idCard`,
		`json:"id_card`, `json:"idNumber`, `json:"address`,
	} {
		if strings.Contains(src, banned) {
			t.Errorf("profile.go 解析了敏感字段 %s——隐私边界必须写死在解析层（只解析 nickname/uid）", banned)
		}
	}
}
