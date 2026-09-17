#!/usr/bin/env bash
# login.sh — WorkBuddy CN OAuth 登录 → 落盘 auth 文件
#
# 用法:
#   ./login.sh
#
# 流程:
#   1. POST /v2/plugin/auth/state 拿授权 URL（无 PKCE，state 由服务端签发）
#   2. 你在浏览器打开 URL 完成登录
#   3. 回到这里按 y → poll 拿 token+uid+nickname → 签到 → 落盘 auths/workbuddy-<uid>.json
#   4. 重启 workbuddy2api 容器加载新账号
set -euo pipefail

cd "$(dirname "$0")"
# 目录可被 WB2A_AUTH_DIR 覆盖：容器内 WB2A_AUTH_DIR=/app/data/auths（持久卷），
# 与 server/credit 同口径——否则容器内登录会写进镜像层 /app/auths，重建即丢。
AUTH_DIR="${WB2A_AUTH_DIR:-./auths}"
CONTAINER="workbuddy2api"

mkdir -p "$AUTH_DIR" 2>/dev/null || true
# ─── auths 目录可写性预检：chown -R 10001 之后宿主机当前 uid 就没有写权限了，
#      此时把整轮 OAuth 走完再在落盘处失败 = 白跑一次浏览器授权。OAuth 启动前 fail-fast。
#      目录不存在时上面的 mkdir 会照常创建，首次登录路径零行为变化。───
if ! [[ -w "$AUTH_DIR" ]]; then
    # 容器内路径（/app/data/...）在宿主机上的对应位置是 compose 卷（默认 ./data）。
    # 报错里给宿主机能直接照抄的路径，否则容器内跑出来的指引在宿主机上执行会 no such file。
    case "$AUTH_DIR" in
        /app/data/*) HINT_DIR="./data/${AUTH_DIR#/app/data/}" ;;
        *)           HINT_DIR="$AUTH_DIR" ;;
    esac
    echo "❌ 无法写入 $AUTH_DIR：OAuth 走完也会在落盘时失败，故此处直接退出" >&2
    echo "    方案 1（推荐）容器内以 app(uid 10001) 身份登录，属主自动正确，无需 chown：" >&2
    echo "      docker compose exec -it wb2api su-exec 10001:10001 ./login.sh" >&2
    echo "    方案 2 宿主机把目录属主交给容器用户：" >&2
    echo "      sudo chown -R 10001:10001 $HINT_DIR" >&2
    echo "    完成后回宿主机重启：docker compose restart wb2api" >&2
    exit 1
fi

# login 工具：不存在才编译（源码改动后手动 go build -o login ./cmd/login）
LOGIN_BIN="./login"
if [[ ! -x "$LOGIN_BIN" ]]; then
    go build -o "$LOGIN_BIN" ./cmd/login
fi

echo "============================================================"
echo "  WorkBuddy OAuth 登录"
echo "============================================================"
echo ""

AUTH_URL=$("$LOGIN_BIN" url)

echo "请在浏览器中打开以下链接完成登录："
echo ""
echo "  $AUTH_URL"
echo ""

if command -v xclip &>/dev/null; then
    echo -n "$AUTH_URL" | xclip -selection clipboard 2>/dev/null && echo "(已复制到剪贴板)"
elif command -v xsel &>/dev/null; then
    echo -n "$AUTH_URL" | xsel --clipboard 2>/dev/null && echo "(已复制到剪贴板)"
fi

echo ""
read -rp "完成登录后按 y 继续: " ans
if [[ "$ans" != "y" && "$ans" != "Y" ]]; then
    echo "已取消"
    exit 1
fi

echo ""
echo "正在获取 token..."

RESULT=$("$LOGIN_BIN" poll) || {
    echo ""
    echo "获取 token 失败。可能原因："
    echo "  - 登录还没完成就按了 y（重新运行 ./login.sh 再试）"
    echo "  - 登录页报错（把报错截图发出来排查）"
    exit 1
}

TOKEN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['access_token'])")
REFRESH=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['refresh_token'])")
EXPIRES_IN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['expires_in'])")
DOMAIN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('domain',''))")
USER_ID=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('uid',''))")
ENT_ID=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('enterprise_id',''))")
NICKNAME=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('nickname',''))")

if [[ -z "$USER_ID" ]]; then
    echo "无法获取 uid，请检查 token 是否有效"
    exit 1
fi

EXPIRES_AT=$(( $(date +%s) + EXPIRES_IN ))

# OAuth 返回字段一律通过环境变量传入 Python，并使用带引号 heredoc：
# 昵称/domain/token 等值可含引号或换行，不得拼进 Python 源码。

# ─── 签到（CN：POST codebuddy.cn/v2/billing/meter/daily-checkin，幂等不阻塞）───
WB2A_LOGIN_TOKEN="$TOKEN" \
WB2A_LOGIN_USER_ID="$USER_ID" \
WB2A_LOGIN_ENT_ID="$ENT_ID" \
WB2A_LOGIN_DOMAIN="$DOMAIN" \
python3 - <<'PYEOF'
import json, os, urllib.request, urllib.error

token = os.environ["WB2A_LOGIN_TOKEN"]
user_id = os.environ["WB2A_LOGIN_USER_ID"]
enterprise_id = os.environ["WB2A_LOGIN_ENT_ID"]
domain = os.environ["WB2A_LOGIN_DOMAIN"]

req = urllib.request.Request(
    "https://www.codebuddy.cn/v2/billing/meter/daily-checkin",
    method="POST", data=b"{}",
    headers={
        "Authorization": "Bearer " + token,
        "Accept": "application/json",
        "Content-Type": "application/json",
        "X-User-Id": user_id,
        **({"X-Enterprise-Id": enterprise_id, "X-Tenant-Id": enterprise_id} if enterprise_id else {}),
        **({"X-Domain": domain} if domain else {}),
    })
try:
    with urllib.request.urlopen(req, timeout=15) as r:
        body = json.loads(r.read().decode() or "{}")
    if body.get("code") == 0:
        data = body.get("data") or {}
        print(f"签到: 成功 {json.dumps(data, ensure_ascii=False)[:150]}")
    else:
        print(f"签到: {body.get('msg', json.dumps(body)[:150])}")
except urllib.error.HTTPError as e:
    # 已签到等业务错误也走 4xx（实测 code=10001 "今天已签到"）
    try:
        body = json.loads(e.read().decode() or "{}")
        print(f"签到: {body.get('msg', 'http %d' % e.code)}")
    except Exception:
        print(f"签到: http {e.code}")
except Exception as e:
    print(f"签到: {e}")
PYEOF

# ─── 落盘 auth 文件（与 internal/auth 读取格式一致）─────────────────
AUTH_FILE="$AUTH_DIR/workbuddy-${USER_ID}.json"
if [[ -f "$AUTH_FILE" ]]; then
    echo "账号已存在（uid=${USER_ID}），将覆盖更新凭证"
    ACTION="覆盖"
else
    echo "新账号（uid=${USER_ID}），新增 auth 文件"
    ACTION="新增"
fi
WB2A_LOGIN_TOKEN="$TOKEN" \
WB2A_LOGIN_REFRESH="$REFRESH" \
WB2A_LOGIN_EXPIRES_AT="$EXPIRES_AT" \
WB2A_LOGIN_DOMAIN="$DOMAIN" \
WB2A_LOGIN_USER_ID="$USER_ID" \
WB2A_LOGIN_ENT_ID="$ENT_ID" \
WB2A_LOGIN_NICKNAME="$NICKNAME" \
WB2A_LOGIN_AUTH_FILE="$AUTH_FILE" \
WB2A_LOGIN_ACTION="$ACTION" \
python3 - <<'PYEOF'
import json, os, sys, tempfile

auth = {
    "account": {
        "uid": os.environ["WB2A_LOGIN_USER_ID"],
        "enterpriseId": os.environ["WB2A_LOGIN_ENT_ID"],
        "nickname": os.environ["WB2A_LOGIN_NICKNAME"]
    },
    "auth": {
        "accessToken": os.environ["WB2A_LOGIN_TOKEN"],
        "refreshToken": os.environ["WB2A_LOGIN_REFRESH"],
        "expiresAt": int(os.environ["WB2A_LOGIN_EXPIRES_AT"]),
        "domain": os.environ["WB2A_LOGIN_DOMAIN"]
    }
}
auth_file = os.environ["WB2A_LOGIN_AUTH_FILE"]
# 容器内 app 运行 uid（docker-entrypoint.sh 以 su-exec 降权到 10001）：属主不匹配会导致
# 账号数为 0（issue #108）。
CONTAINER_UID = 10001
# 容器内路径 /app/data/... 在宿主机上的对应位置是 compose 卷（默认 ./data）：
# 指引里给宿主机能直接照抄的路径，否则容器内跑出来的报错在宿主机上执行会 no such file。
def host_hint(path):
    if path.startswith("/app/data/"):
        return "./data/" + path[len("/app/data/"):]
    return path
# mkstemp 必须在 try 块内（issue #160）：目录不可写时它第一个抛 PermissionError，
# 放在 try 外会使下方失败指引成为死代码（裸 traceback 直接冒出）。
tmp_file = None
try:
    fd, tmp_file = tempfile.mkstemp(prefix=".workbuddy-auth-", dir=os.path.dirname(auth_file) or ".")
    with os.fdopen(fd, "w") as f:
        json.dump(auth, f, indent=1)
    os.replace(tmp_file, auth_file)
    # 权限检测：容器内 app(uid 10001) 需要读此文件
    st = os.stat(auth_file)
    if st.st_uid != CONTAINER_UID:
        print(f"\n⚠️  权限警告：{auth_file} 属主 uid={st.st_uid}，容器内 app(uid={CONTAINER_UID}) 可能读不到")
        print(f"    请执行：sudo chown -R {CONTAINER_UID}:{CONTAINER_UID} {host_hint(auth_file)}")
        print(f"    或以 app 身份在容器内登录（属主自动正确）：")
        print(f"    docker compose exec -it wb2api su-exec {CONTAINER_UID}:{CONTAINER_UID} ./login.sh\n")
except Exception:
    if tmp_file is not None:
        try:
            os.unlink(tmp_file)
        except FileNotFoundError:
            pass
    # 写入失败诊断：目录不可写时给出具体指引
    auth_dir = os.path.dirname(auth_file) or "."
    if not os.access(auth_dir, os.W_OK):
        print(f"\n❌ 写入失败：目录 {auth_dir} 不可写（权限不足）", file=sys.stderr)
        print(f"    请执行：sudo chown -R {CONTAINER_UID}:{CONTAINER_UID} {host_hint(auth_dir)}", file=sys.stderr)
        print(f"    或以 app 身份在容器内登录：", file=sys.stderr)
        print(f"    docker compose exec -it wb2api su-exec {CONTAINER_UID}:{CONTAINER_UID} ./login.sh\n", file=sys.stderr)
    raise
print(f"已保存（{os.environ['WB2A_LOGIN_ACTION']}）: {auth_file}")
PYEOF

# ─── 重启服务 ────────────────────────────────────────────
echo ""
if docker ps --format '{{.Names}}' | grep -q "^${CONTAINER}$"; then
    echo "重启 $CONTAINER 加载新账号..."
    docker restart "$CONTAINER" >/dev/null
    sleep 2
    # API_KEY 从 config.json 读取（该变量在脚本中未定义，fallback 仅为占位，不会通过鉴权）
    API_KEY=$(python3 -c "import json; print(json.load(open('config.json')).get('api_key',''))" 2>/dev/null)
    COUNT=$(curl -s http://127.0.0.1:7863/status -H "Authorization: Bearer ${API_KEY:-test_key}" 2>/dev/null | python3 -c "import json,sys; print(len(json.load(sys.stdin).get('accounts',[])))" 2>/dev/null || echo "?")
    echo "服务已重启，当前账号数: $COUNT"
else
    echo "容器 $CONTAINER 未运行，auth 文件已保存，下次启动自动加载"
fi

echo ""
echo "============================================================"
echo "  登录完成！"
echo "  UID: $USER_ID"
echo "  Nickname: ${NICKNAME:-（未获取到）}"
echo "  Token: ${TOKEN:0:30}..."
echo "  有效期: $(date -d "@$EXPIRES_AT" '+%Y-%m-%d %H:%M' 2>/dev/null || echo "$EXPIRES_AT")"
echo "============================================================"
