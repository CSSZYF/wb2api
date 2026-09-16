#!/bin/sh
# 容器入口：修正持久卷属主 → 兼容旧配置布局 → 降权执行。
#
# 为什么需要它：镜像里的服务以 uid 10001 运行，而持久卷由平台/Docker 创建时
# 通常是 root:root。属主不匹配的直接后果是"能启动但写不了盘"——面板保存配置报
# permission denied、账号凭证落盘失败、用量记录写不进去。这些都不是崩溃，
# 而是静默失败，排查起来最费时间，所以在入口一次性处理掉。
set -e

DATA_DIR="${WB2A_DATA_DIR:-/app/data}"
CONFIG="$DATA_DIR/config.json"
APP_UID=10001
APP_GID=10001

# ── 1) 修正持久卷属主（仅在以 root 进入时可能成功）──
if [ "$(id -u)" = "0" ]; then
  mkdir -p "$DATA_DIR" 2>/dev/null || true
  chown -R "$APP_UID:$APP_GID" "$DATA_DIR" 2>/dev/null || true
fi

if [ ! -w "$DATA_DIR" ]; then
  echo "[entrypoint] 警告：$DATA_DIR 不可写（当前 uid=$(id -u)）。" >&2
  echo "[entrypoint]  面板保存配置、账号凭证落盘都会失败。" >&2
  echo "[entrypoint]  处理：让容器以 root 启动（本入口会自行降权到 $APP_UID），" >&2
  echo "[entrypoint]  或在宿主机执行 chown -R $APP_UID:$APP_GID <卷目录>。" >&2
fi

# ── 2) 兼容旧配置布局 ──
# v1.9.3 及以前配置在 /app/config.json（宿主挂载点），现改为落在持久卷上。
# 只有当旧文件与镜像自带示例**逐字节不同**时才迁移：与示例相同说明没人动过它，
# 把一份 api_key=test_key 的示例搬进卷里，只会制造"看起来像真配置"的假象；
# 不如留给二进制自己生成（它会写入一个随机 api_key）。
if [ ! -f "$CONFIG" ] && [ -f /app/config.json ] && ! cmp -s /app/config.json /app/config.example.json; then
  cp /app/config.json "$CONFIG"
  echo "[entrypoint] 已迁移旧配置：/app/config.json -> $CONFIG"
  if [ "$(id -u)" = "0" ]; then
    chown "$APP_UID:$APP_GID" "$CONFIG" 2>/dev/null || true
  fi
fi

# ── 3) 降权执行 ──
# 服务进程本身不以 root 运行（与旧镜像的 USER app 等效），只是入口脚本需要 root
# 才能修正卷属主。平台若强制非 root 启动，这里直接 exec，不做多余动作。
if [ "$(id -u)" = "0" ]; then
  exec su-exec "$APP_UID:$APP_GID" "$@"
fi
exec "$@"
