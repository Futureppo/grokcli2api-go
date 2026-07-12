#!/usr/bin/env bash

set -Eeuo pipefail

REPOSITORY="${REPOSITORY:-Futureppo/grokcli2api-go}"
DEPLOY_REF="${DEPLOY_REF:-main}"
INSTALL_DIR="${INSTALL_DIR:-${HOME}/grokcli2api-go}"
PORT="${GROK2API_PORT:-8088}"
ENABLE_ADMIN_API="${ENABLE_ADMIN_API:-1}"
RAW_BASE="https://raw.githubusercontent.com/${REPOSITORY}/${DEPLOY_REF}"

info() {
  printf '\033[1;34m[INFO]\033[0m %s\n' "$*"
}

success() {
  printf '\033[1;32m[OK]\033[0m %s\n' "$*"
}

warn() {
  printf '\033[1;33m[WARN]\033[0m %s\n' "$*" >&2
}

fail() {
  printf '\033[1;31m[ERROR]\033[0m %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "缺少命令：$1"
}

fetch() {
  local url="$1"
  local output="$2"
  curl --fail --silent --show-error --location --retry 3 "$url" --output "$output"
}

read_env_value() {
  local key="$1"
  local file="$2"
  awk -v key="$key" '
    index($0, key "=") == 1 {
      sub(/^[^=]*=/, "")
      print
      exit
    }
  ' "$file"
}

upsert_env() {
  local key="$1"
  local value="$2"
  local file="$3"
  local temporary
  temporary="$(mktemp "${file}.XXXXXX")"
  awk -v key="$key" -v value="$value" '
    BEGIN { replaced = 0 }
    index($0, key "=") == 1 {
      if (!replaced) {
        print key "=" value
        replaced = 1
      }
      next
    }
    { print }
    END {
      if (!replaced) {
        print key "=" value
      }
    }
  ' "$file" >"$temporary"
  mv "$temporary" "$file"
}

generate_secret() {
  local prefix="$1"
  if command -v openssl >/dev/null 2>&1; then
    printf '%s%s' "$prefix" "$(openssl rand -hex 24)"
    return
  fi
  printf '%s%s' "$prefix" "$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')"
}

find_credential() {
  find "$1" -maxdepth 1 -type f -name '*.json' -print -quit 2>/dev/null || true
}

case "$PORT" in
  ''|*[!0-9]*) fail "GROK2API_PORT 必须是 1 到 65535 之间的整数" ;;
esac
if ((PORT < 1 || PORT > 65535)); then
  fail "GROK2API_PORT 必须是 1 到 65535 之间的整数"
fi
case "$ENABLE_ADMIN_API" in
  0|1) ;;
  *) fail "ENABLE_ADMIN_API 只能设置为 0 或 1" ;;
esac

require_command curl
require_command docker
require_command awk
require_command find

docker compose version >/dev/null 2>&1 || fail "未检测到 Docker Compose v2，请先安装或升级 Docker"
docker info >/dev/null 2>&1 || fail "Docker 服务未运行，或当前用户没有访问 Docker 的权限"

mkdir -p "$INSTALL_DIR/auths"
chmod 700 "$INSTALL_DIR/auths"

compose_tmp="$(mktemp "${INSTALL_DIR}/compose.yaml.XXXXXX")"
env_tmp=""
trap 'rm -f "$compose_tmp" "$env_tmp"' EXIT

info "下载 Docker Compose 配置"
fetch "${RAW_BASE}/compose.yaml" "$compose_tmp"
mv "$compose_tmp" "$INSTALL_DIR/compose.yaml"

if [[ ! -f "$INSTALL_DIR/.env" ]]; then
  env_tmp="$(mktemp "${INSTALL_DIR}/.env.XXXXXX")"
  info "创建环境变量配置"
  fetch "${RAW_BASE}/.env.example" "$env_tmp"
  mv "$env_tmp" "$INSTALL_DIR/.env"
else
  info "保留现有 .env 配置"
fi

api_keys="${GROK_API_KEYS:-}"
if [[ -z "$api_keys" ]]; then
  api_keys="$(read_env_value GROK_API_KEYS "$INSTALL_DIR/.env")"
fi
if [[ -z "$api_keys" || "$api_keys" == "key1,key2,key3" ]]; then
  api_keys="$(generate_secret 'sk-')"
  generated_api_key=1
else
  generated_api_key=0
fi
if [[ "$api_keys" == *$'\n'* || "$api_keys" == *$'\r'* ]]; then
  fail "GROK_API_KEYS 不能包含换行符"
fi

admin_key="${GROK_ADMIN_KEY:-}"
if [[ -z "$admin_key" ]]; then
  admin_key="$(read_env_value GROK_ADMIN_KEY "$INSTALL_DIR/.env")"
fi
if [[ "$ENABLE_ADMIN_API" == "1" ]]; then
  if [[ -z "$admin_key" || "$admin_key" == "admin" ]]; then
    admin_key="$(generate_secret 'adm-')"
    generated_admin_key=1
  else
    generated_admin_key=0
  fi
else
  admin_key=""
  generated_admin_key=0
fi
if [[ "$admin_key" == *$'\n'* || "$admin_key" == *$'\r'* ]]; then
  fail "GROK_ADMIN_KEY 不能包含换行符"
fi

upsert_env GROK2API_PORT "$PORT" "$INSTALL_DIR/.env"
upsert_env GROK_API_KEYS "$api_keys" "$INSTALL_DIR/.env"
upsert_env GROK_ADMIN_KEY "$admin_key" "$INSTALL_DIR/.env"
chmod 600 "$INSTALL_DIR/.env"

credential="$(find_credential "$INSTALL_DIR/auths")"
credential_source="${AUTH_FILE:-}"

if [[ -z "$credential" && -z "$credential_source" && -t 1 && -r /dev/tty ]]; then
  printf '请输入 Grok CLI OAuth JSON 凭证路径（直接回车后可通过管理 API 上传）：' >/dev/tty
  IFS= read -r credential_source </dev/tty || true
fi

if [[ -n "$credential_source" ]]; then
  [[ -f "$credential_source" ]] || fail "找不到凭证文件：$credential_source"
  credential_name="$(basename "$credential_source")"
  [[ "$credential_name" == *.json ]] || credential_name="${credential_name}.json"
  cp "$credential_source" "$INSTALL_DIR/auths/$credential_name"
  chmod 600 "$INSTALL_DIR/auths/$credential_name"
  credential="$INSTALL_DIR/auths/$credential_name"
  success "已导入凭证：auths/$credential_name"
fi

if [[ -z "$credential" && "$ENABLE_ADMIN_API" == "0" ]]; then
  warn "尚未发现 OAuth JSON 凭证，且管理员 API 已关闭，因此服务没有启动。"
  printf '\n将凭证放入 %s/auths/ 后重新执行，或设置 ENABLE_ADMIN_API=1。\n' "$INSTALL_DIR"
  exit 0
fi
if [[ -z "$credential" ]]; then
  warn "当前没有 OAuth JSON 凭证；服务将以空凭证池启动，请随后通过管理员 API 上传。"
fi

info "拉取镜像并启动服务"
(
  cd "$INSTALL_DIR"
  docker compose pull
  docker compose up -d --remove-orphans
)

info "等待健康检查"
ready=0
for ((attempt = 1; attempt <= 30; attempt++)); do
  if curl --fail --silent --max-time 2 "http://127.0.0.1:${PORT}/" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 2
done

if ((ready == 0)); then
  warn "服务未在预期时间内就绪，最近的容器日志如下："
  (cd "$INSTALL_DIR" && docker compose logs --tail 80) || true
  fail "部署未通过健康检查"
fi

if [[ "$ENABLE_ADMIN_API" == "1" ]]; then
  if ! curl --fail --silent --max-time 5 \
    -H "X-Admin-Key: ${admin_key}" \
    "http://127.0.0.1:${PORT}/v1/admin/credentials" >/dev/null; then
    fail "服务已启动，但管理员 API 验证失败"
  fi
  success "管理员 API 验证通过"
fi

success "grokcli2api-go 已启动：http://127.0.0.1:${PORT}"
printf '安装目录：%s\n' "$INSTALL_DIR"
if ((generated_api_key == 1)); then
  printf '自动生成的本地 API Key：%s\n' "$api_keys"
  warn "请立即保存此 Key；后续可在 ${INSTALL_DIR}/.env 中查看或更换。"
else
  printf '本地 API Key 已按现有配置启用。\n'
fi
if [[ "$ENABLE_ADMIN_API" == "1" ]]; then
  if ((generated_admin_key == 1)); then
    printf '自动生成的管理员 Key：%s\n' "$admin_key"
    warn "请立即保存管理员 Key，并且只通过 HTTPS 或本机回环地址使用管理接口。"
  else
    printf '管理员 API 已使用现有独立密钥启用。\n'
  fi
  printf '\n凭证管理示例：\n'
  printf '  上传：curl http://127.0.0.1:%s/v1/admin/credentials -H "X-Admin-Key: <管理员Key>" -F "file=@/path/to/account.json"\n' "$PORT"
  printf '  列表：curl http://127.0.0.1:%s/v1/admin/credentials -H "X-Admin-Key: <管理员Key>"\n' "$PORT"
fi
printf '\n常用命令：\n'
printf '  查看状态：cd %q && docker compose ps\n' "$INSTALL_DIR"
printf '  查看日志：cd %q && docker compose logs -f\n' "$INSTALL_DIR"
printf '  更新服务：重新执行本部署脚本\n'
