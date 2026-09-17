#!/usr/bin/env bash
# test_login_sh.sh — login.sh 加固的回归测试（防注入 / 可写性预检 / 原子落盘）
#
# 仅 Linux（POSIX 权限语义 + setpriv）；在 Windows 上无法复现权限场景，跳过。
#
# T1：落盘段在目录不可写时打印指引而非裸 traceback（mkstemp 在 try 块内，
#     失败指引可达）。从 login.sh 源码提取 heredoc 原样 exec（零逻辑副本）。
# T2：auths 目录不可写时 login.sh 在 OAuth 流程启动前 fail-fast。
# T3（对照）：目录可写时预检不拦截，OAuth 启动段可达（防误伤）。
# T4：OAuth 返回的 nickname/token 含引号、反斜杠、$、换行等注入载荷时，
#     落盘的 JSON 逐字节等于期望值（值经环境变量传入，不拼进 Python 源码）。
#
# T1/T2 不能用 root 复现（root 对任何目录 -w 恒真），用 setpriv 切到 uid 12345
# 并以其为属主构造真实权限语义。
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOGIN_SH="${LOGIN_SH:-$REPO_ROOT/login.sh}"

if [[ "$(uname -s)" == MINGW* || "$(uname -s)" == MSYS* || "$(uname -s)" == CYGWIN* ]]; then
    echo "skip: Windows 无 POSIX 权限语义（setpriv/chmod 555 无效），请在 Linux/WSL 下运行"
    exit 0
fi

# T1/T2 不能用 root 复现（root 对任何目录 -w 恒真），需要一个非 root uid 来构造
# 真实权限语义：已是非 root 就直接用当前 uid（mktemp 建的目录本就归自己，无需
# chown）；以 root 运行时切到 NONROOT_UID，并把目录属主交给它。
if [[ "$(id -u)" -eq 0 ]]; then
    NONROOT_UID="${NONROOT_UID:-12345}"
    NONROOT="setpriv --reuid $NONROOT_UID --regid $NONROOT_UID --clear-groups"
    NEED_CHOWN=1
else
    NONROOT_UID="$(id -u)"
    NONROOT=""
    NEED_CHOWN=0
fi

PASS=0
FAIL=0

say()  { printf '%s\n' "$*"; }
ok()   { say "  ✓ $*"; PASS=$((PASS + 1)); }
fail() { say "  ✗ $*"; FAIL=$((FAIL + 1)); }

setup_dir() {
    local dir
    dir=$(mktemp -d /tmp/wb-login-test.XXXXXX)
    mkdir -p "$dir/auths"
    [[ "$NEED_CHOWN" -eq 1 ]] && chown -R "$NONROOT_UID:$NONROOT_UID" "$dir"
    printf '%s' "$dir"
}

# 以非 root 身份跑命令：root 场景走 setpriv 降权，非 root 场景直接跑。
as_nonroot() {
    if [[ -n "$NONROOT" ]]; then
        $NONROOT "$@"
    else
        "$@"
    fi
}

# 落盘段提取器：从 login.sh 源码里取出最后一个 heredoc（= 落盘段）的 Python 体并
# exec。零逻辑副本——login.sh 改了测试跟着走。用正则匹配而非固定起始串，新旧实现
# 都能提取，便于对旧实现跑同一套断言拿 RED 证据。
write_extractor() {
    local f
    f=$(mktemp /tmp/wb-login-stage-exec.XXXXXX.py)
    cat >"$f" <<'PYEOF'
import os, re

login_sh = os.environ["WB2A_LOGIN_TEST_SH"]
src = open(login_sh, encoding="utf-8").read()

blocks = re.findall(r"python3 - <<'?PYEOF'?\n(.*?)\nPYEOF", src, re.S)
if not blocks:
    raise SystemExit("未在 %s 中找到 heredoc 落盘段" % login_sh)
body = blocks[-1]
exec(compile(body, "login-auth-stage", "exec"), {"__name__": "__main__"})
PYEOF
    chmod 644 "$f"
    printf '%s' "$f"
}

run_write_stage() {
    local login_sh="$1" auth_dir="$2" extractor="$3" nickname="${4:-test}"
    WB2A_LOGIN_TOKEN=tok \
    WB2A_LOGIN_REFRESH=ref \
    WB2A_LOGIN_EXPIRES_AT=1893456000 \
    WB2A_LOGIN_DOMAIN=www.codebuddy.cn \
    WB2A_LOGIN_USER_ID=12345 \
    WB2A_LOGIN_ENT_ID='' \
    WB2A_LOGIN_NICKNAME="$nickname" \
    WB2A_LOGIN_AUTH_FILE="$auth_dir/auths/workbuddy-12345.json" \
    WB2A_LOGIN_ACTION=新增 \
    WB2A_LOGIN_TEST_SH="$login_sh" \
    as_nonroot python3 "$extractor" 2>&1
}

# ─── T1：写入失败时指引可达（mkstemp 在 try 内）────────────────────
t1() {
    say "T1: 落盘段目录不可写 → 打印指引而非裸 traceback"
    local dir rc extractor login_copy
    dir=$(setup_dir)
    chmod 555 "$dir/auths"   # 属主也失去写权限（模拟 chown 给了别的 uid）
    extractor=$(write_extractor)
    login_copy="$dir/login.sh"   # 仓库内文件对 uid 12345 未必可读，复制成 644
    cp "$LOGIN_SH" "$login_copy"
    chmod 644 "$login_copy"

    set +e
    OUT=$(run_write_stage "$login_copy" "$dir" "$extractor" 2>&1)
    rc=$?
    set -e

    if [[ $rc -ne 0 ]]; then
        ok "落盘段以非零退出码失败（rc=$rc）"
    else
        fail "落盘段意外成功"
    fi
    if grep -q "不可写" <<<"$OUT"; then
        ok "输出含「不可写」指引文案"
    else
        fail "缺少「不可写」指引（诊断仍是死代码？）"
    fi
    if grep -q "Traceback" <<<"$OUT"; then
        ok "Traceback 仍在（raise 保留，不吞异常）"
    else
        fail "Traceback 消失了（不应吞异常）"
    fi
    if grep -q "chown -R 10001:10001" <<<"$OUT"; then
        ok "指引含 chown 命令"
    else
        fail "指引缺少 chown 命令"
    fi
    rm -rf "$dir" "$extractor"
}

# ─── T2：预检在 OAuth 启动前退出 ───────────────────────────────────
# stub login 二进制每次被调（url/poll）都先写 marker：marker 出现 = OAuth 已启动
# （预检失效）；marker 未出现且退出码非 0 + 打印指引 = 预检正确挡在浏览器流程之前。
# 注意 auths 必须预先存在：login.sh 的 mkdir -p 对已存在目录幂等（不修权限），
# 若不存在会把它创建成可写，预检就测不到了。
t2() {
    say "T2: auths 不可写 → OAuth 启动前 fail-fast"
    local dir rc marker
    dir=$(setup_dir)
    marker="$dir/.oauth-started"

    mkdir -p "$dir/skel/auths"
    cp "$LOGIN_SH" "$dir/skel/login.sh"
    cat > "$dir/skel/login" <<'STUB'
#!/usr/bin/env bash
# OAuth 流程 stub：login.sh 会先 $LOGIN_BIN url 拿授权 URL。真实流程此处必然在
# 浏览器打开后才会往下走——这里写 marker 证明「已进入 OAuth 启动段」。
echo "stub-url-called" >> "${WB2A_LOGIN_OAUTH_MARKER:-/dev/null}"
if [[ "${1:-}" == "url" ]]; then echo "https://stub.invalid/auth"; exit 0; fi
exit 0
STUB
    chmod 755 "$dir/skel/login" "$dir/skel/login.sh"
    [[ "$NEED_CHOWN" -eq 1 ]] && chown -R "$NONROOT_UID:$NONROOT_UID" "$dir/skel"
    chmod 555 "$dir/skel/auths"   # auths 不可写

    set +e
    OUT=$(cd "$dir/skel" && WB2A_LOGIN_OAUTH_MARKER="$marker" \
        as_nonroot bash ./login.sh 2>&1 </dev/null)
    rc=$?
    set -e

    if [[ $rc -ne 0 ]]; then
        ok "预检以非零退出码拦截（rc=$rc）"
    else
        fail "login.sh 意外走完（预检未生效）"
    fi
    if grep -q "无法写入" <<<"$OUT"; then
        ok "输出含「无法写入」预检文案"
    else
        fail "缺少预检指引文案"
    fi
    if grep -q "docker compose exec -it wb2api" <<<"$OUT"; then
        ok "指引含容器内登录命令"
    else
        fail "指引缺少容器内登录命令"
    fi
    if [[ ! -e "$marker" ]]; then
        ok "未进入 OAuth 启动段（marker 未写）"
    else
        fail "OAuth 流程已启动（预检晚于浏览器流程）"
    fi
    if compgen -G "$dir/skel/auths/workbuddy-*.json" >/dev/null; then
        fail "意外写入了 auth 文件"
    else
        ok "未落盘任何 auth 文件"
    fi
    rm -rf "$dir"
}

# ─── T3（对照）：目录可写时预检不拦截 ───────────────────────────────
t3() {
    say "T3（对照）: auths 可写 → 预检不拦截，OAuth 启动段可达"
    local dir marker
    dir=$(setup_dir)
    marker="$dir/.oauth-started"
    mkdir -p "$dir/skel/auths"
    cp "$LOGIN_SH" "$dir/skel/login.sh"
    cat > "$dir/skel/login" <<'STUB'
#!/usr/bin/env bash
echo "stub-url-called" >> "${WB2A_LOGIN_OAUTH_MARKER:-/dev/null}"
if [[ "${1:-}" == "url" ]]; then echo "https://stub.invalid/auth"; exit 0; fi
exit 1   # poll 阶段故意失败：只需走到 OAuth 启动，无需真 token
STUB
    chmod 755 "$dir/skel/login"
    [[ "$NEED_CHOWN" -eq 1 ]] && chown -R "$NONROOT_UID:$NONROOT_UID" "$dir/skel"

    set +e
    # login.sh 会 read -rp 等 y：stdin 给 /dev/null（非 tty），read 立即 EOF → 取消退出
    OUT=$(cd "$dir/skel" && WB2A_LOGIN_OAUTH_MARKER="$marker" \
        as_nonroot bash ./login.sh </dev/null 2>&1 | head -20)
    set -e

    if [[ -e "$marker" ]]; then
        ok "OAuth 启动段可达（happy path 未被误伤）"
    else
        fail "可写目录也被预检拦截（误伤）"
    fi
    if grep -q "无法写入" <<<"$OUT"; then
        fail "可写目录误报不可写"
    else
        ok "无「无法写入」误报"
    fi
    rm -rf "$dir"
}

# ─── T4：注入载荷经环境变量传入，落盘 JSON 逐字节正确 ───────────────
# 载荷含引号 / 反斜杠 / $ / 换行 / Python 语法片段：旧实现把它们拼进 Python 源码，
# 轻则语法错（落盘失败），重则执行任意代码。断言落盘值 == 期望值。
t4() {
    say "T4: 昵称含引号/换行/\$ 等载荷 → 落盘 JSON 值逐字节一致（防注入）"
    local dir extractor login_copy payload rc
    dir=$(setup_dir)
    extractor=$(write_extractor)
    login_copy="$dir/login.sh"
    cp "$LOGIN_SH" "$login_copy"
    chmod 644 "$login_copy"

    # 载荷：双引号 + 反斜杠 + $ + 换行 + Python 代码片段
    payload=$(printf 'a"b\\c$d\n_e %s' '") ; import os; os.system("touch /tmp/pwned") #')

    set +e
    OUT=$(run_write_stage "$login_copy" "$dir" "$extractor" "$payload" 2>&1)
    rc=$?
    set -e

    if [[ $rc -ne 0 ]]; then
        fail "含载荷的落盘意外失败（rc=$rc）：$OUT"
        rm -rf "$dir" "$extractor"
        return
    fi

    # 用 Python 直接比对落盘文件里的 nickname 与原始载荷。
    set +e
    DIFF=$(WB2A_EXPECT="$payload" python3 - "$dir/auths/workbuddy-12345.json" <<'PYEOF' 2>&1
import json, os, sys
with open(sys.argv[1], encoding="utf-8") as f:
    doc = json.load(f)
got = doc["account"]["nickname"]
want = os.environ["WB2A_EXPECT"]
if got != want:
    print("MISMATCH got=%r want=%r" % (got, want))
    sys.exit(1)
if not doc["auth"]["accessToken"]:
    print("accessToken 丢失")
    sys.exit(1)
PYEOF
)
    rc=$?
    set -e
    if [[ $rc -eq 0 ]]; then
        ok "载荷原样落盘（未被解释执行 / 未破坏 JSON）"
    else
        fail "落盘值与输入不一致：$DIFF"
    fi

    # 目录里不应有残留临时文件（原子写 + 失败清理）。
    if compgen -G "$dir/auths/.*tmp*" >/dev/null; then
        fail "auths 目录残留临时文件"
    else
        ok "无残留临时文件"
    fi
    rm -rf "$dir" "$extractor"
}

say "login.sh 回归测试：LOGIN_SH=$LOGIN_SH"
t1
t2
t3
t4
say ""
say "通过: $PASS  失败: $FAIL"
[[ $FAIL -eq 0 ]] || exit 1
