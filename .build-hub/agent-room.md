# agent-room Build Hub deployment

- Service ID: `3cb98176-df65-46d4-b80a-95d89516c845`
- Service name: `agent-room`
- Owner: `zhoushoujian`
- VM: `ubuntu@10.150.115.200`
- Domain: `https://agent-room-zhoushoujian.build.patsnap.info`
- Access mode: `intranet`
- Runtime: `go1.26.3-linux-amd64`
- Backend port: `31080`
- Backend command: `agent-room relay -addr :$PORT -db ~/apps/agent-room/shared/db/agent-room.db`
- Layout: `~/apps/agent-room/current -> releases/<version>`; packages and releases keep the latest 5.
- Database: sqlite at `~/apps/agent-room/shared/db/agent-room.db`
- Backups: enabled under `~/apps/agent-room/shared/backups/`; predeploy backup created for each manual release.
- SSO: enabled through Build Hub, with `AGENT_ROOM_PUBLIC_BASE_URL=https://agent-room-zhoushoujian.build.patsnap.info` in the remote environment.
- Last deployed version: `20260623-105224-reply-context-relay-fallback`
- Last deployed at: `2026-06-23 10:54 Asia/Shanghai`

## 发布规则

- 以后说“发布”默认同步两个平台：Build Hub 应用服务和 GitHub `main` 分支。
- Build Hub 发布包括：本机交叉编译 Linux amd64 二进制、SSH 上传版本 tar 包、切换 VM `current` 软链、重启 `agent-room.service`、验证 `/healthz`、调用 Build Hub register/PATCH 写入发布事件和最新 metadata。
- GitHub 发布包括：提交源码改动、前端 dist 产物、测试更新和部署记录，然后推送到 `origin/main`。

## 踩坑笔记

- 2026-06-17: VM 的非交互 shell 没有可用 `go` 命令；发布时用本机 `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build` 交叉编译后上传到 release 的 `bin/agent-room`。
- 2026-06-22: `register.sh` 在当前环境触发 `TAGS[*]: unbound variable`，本次改用 Build Hub `PATCH /api/v1/services/:id` 完成发布登记；传 `backend_cmd` 时必须用单引号保留字面量 `$PORT`。
- 2026-06-23: 引用回复问题的第一次修复只覆盖新 agent prompt；线上旧 agent `20260613-023309-8206bfb` 仍不读取 `metadata.reply_excerpt`。relay 侧需要给目标 agent 下发私有增强帧，把引用上下文注入 `content`，同时保持历史记录和普通用户广播为原文。
