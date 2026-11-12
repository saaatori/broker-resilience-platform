# 排障手册（Troubleshooting）

> 记录开发过程中实际踩过的坑：现象 → 原因 → 解决。按时间自然积累，同类的坑归到同一节。
> 本文档同时是论文附录的工程实践素材。

---

## 容器与 Docker

### 1. PostgreSQL 18 容器启动即崩溃重启（2026-11-08）

**现象**：`brp-postgres` 状态 `Restarting (1)`，日志输出大段说明并以 `Error: in 18+, these Docker images are configured to store database data...` 结束。

**原因**：PostgreSQL 18 官方镜像的破坏性变更——数据目录改为版本化子目录结构，要求卷挂载到父目录 `/var/lib/postgresql`；旧惯例挂载 `/var/lib/postgresql/data` 会被识别为"升级残留"而拒绝启动（为 pg_upgrade --link 兼容性设计）。

**解决**：compose 中卷改为 `brp_pgdata:/var/lib/postgresql`；因首次初始化失败过，需 `docker compose down -v` 清卷后重建。

### 2. docker 命令报 npipe 连接失败（2026-11-08）

**现象**：`failed to connect to the docker API at npipe:////./pipe/dockerDesktopLinuxEngine`。

**原因**：Docker Desktop 未运行（WSL2 后端的守护进程不在）。

**解决**：启动 Docker Desktop 后等待托盘图标稳定（或轮询 `docker info` 直到成功）再执行 compose 命令。

## Go 工具链

### 3. go install 报 dial tcp 127.0.0.1:443（2026-11-08）

**现象**：`go install ...@latest` 失败：`Get "https://proxy.golang.org/...": dial tcp 127.0.0.1:443: connectex: No connection could be made...`

**原因**：两个因素叠加：① GOPROXY 仍为默认的 proxy.golang.org；② 环境残留 `HTTP_PROXY/HTTPS_PROXY` 指向本机未运行的代理（127.0.0.1），请求根本没出本机。

**解决**：`go env -w GOPROXY=https://goproxy.cn,direct` + `go env -w HTTP_PROXY= HTTPS_PROXY=`（并清当前会话变量）。若仍报 127.0.0.1，检查 hosts 文件是否把 proxy.golang.org 指向了本机。

### 4. 终端找不到 go 命令（2026-11-08）

**现象**：Git Bash 中 `go: command not found`，但新开 PowerShell 正常。

**原因**：Go 安装（本机装在 `F:\Go`）写入的是系统 PATH，安装前已开着的终端会话不会自动刷新。

**解决**：重开终端；或临时 `export PATH=/f/Go/bin:$PATH`。同类问题适用于所有新装 CLI（go install 装的工具在 `%USERPROFILE%\go\bin`，需确认该目录已入 PATH）。

## Python 环境

### 5. pip 装包位置与预期不符（2026-11-08）

**现象**：PowerShell 提示符带 `(base)`，`pip install` 装进了 Miniconda base 而非独立安装的 Python 3.13。

**原因**：本机双 Python 并存（独立 3.13 + Miniconda），conda init 默认自动激活 base，激活态下 `python/pip` 指向 conda。

**解决**：`conda config --set auto_activate_base false` 后重开终端（对旧窗口不生效），确认 `pip --version` 指向独立 3.13。项目依赖一律走 `uv` + 仓库内 `.venv`，与全局环境解耦。

## 数据库客户端

### 6. DBeaver 看不到新建的业务库（2026-11-08）

**现象**：连接成功但左侧树只有 `postgres` 库，没有 `broker_resilience`。

**原因**：DBeaver 对 PostgreSQL 默认只显示连接配置中指定的那一个库。

**解决**：编辑连接 → PostgreSQL 标签页 → 勾选 **Show all databases** → 刷新。`docker exec brp-postgres psql -U brp -l` 可随时佐证库是否存在。

## 脚本与工具

### 7. Git Bash 下 base64 解码 JWT 退出码非 0（2026-11-10）

**现象**：验收脚本（`set -e`）执行到 `PAYLOAD=$(echo "$TOKEN" | cut -d. -f2 | tr '_-' '/+' | base64 -d 2>/dev/null)` 一行后**静默中断**，无任何报错；payload 内容实际已完整解出。

**原因**：JWT 的 base64url 编码不带 `=` padding，段长非 4 的倍数时 coreutils `base64 -d` 会解出完整内容但以非 0 退出码结束；stderr 被丢弃、命令替换赋值语句在 `set -e` 下直接终止脚本。

**解决**：管道尾加 `|| true` 容忍退出码，解码内容的正确性改由后续 `grep` 校验；手工排查时直接看输出、无视退出码即可。

### 8. Jenkins 拉取 GitHub 超时 / fetch master 分支（2026-11-12）

**现象**：from-SCM 管线首跑即失败：`Failed to connect to github.com port 443`，且日志里 fetch 的是 `refs/heads/master`（仓库默认分支是 main）。

**原因**：两个独立问题叠加。① Jenkins 服务以 **LocalSystem** 运行，读不到用户级配置——用户 git 能通是因为 `~/.gitconfig` 配了 `http.proxy=127.0.0.1:7890`（clash），服务账户的 git 走直连被墙；② job 的 Branch Specifier 留了默认值 `*/master`。

**解决**：① 代理写入 **系统级 git 配置**（`git config --system http.proxy/https.proxy http://127.0.0.1:7890`，写入 `D:\Git\Git\etc\gitconfig`，对所有账户生效，无需重启服务/改服务环境变量）；② Branch Specifier 改 `*/main`。**注意：clash 必须在运行**，含每 10 分钟的 Poll SCM，代理关闭时管线会在 checkout 阶段红。

### 9. 误把文件暂存进上一次提交（2026-11-12）

**现象**：`Jenkinsfile` 被误包含进 `fix(backend)` 提交（已提交未推送）。

**解决**：未推送的提交可安全重写：`git reset --soft HEAD~1`（改动退回暂存区）→ `git restore --staged Jenkinsfile`（移出暂存）→ 重新提交正确内容。已推送则不建议改历史。
