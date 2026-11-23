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

**原因**：两个独立问题叠加。① Jenkins 服务以 **LocalSystem** 运行，读不到用户级配置——用户 git 能通是因为 `~/.gitconfig` 配了 `http.proxy=127.0.0.1:7890`（本地代理客户端：龙猫云，clash 系内核默认端口），服务账户的 git 走直连被墙；② job 的 Branch Specifier 留了默认值 `*/master`。

**解决**：① 代理写入 **系统级 git 配置**（`git config --system http.proxy/https.proxy http://127.0.0.1:7890`，写入 `D:\Git\Git\etc\gitconfig`，对所有账户生效，无需重启服务/改服务环境变量）；② Branch Specifier 改 `*/main`。**注意：龙猫云代理客户端必须在运行**（127.0.0.1:7890），含每 10 分钟的 Poll SCM，代理关闭时管线会在 checkout 阶段红。

### 9. 误把文件暂存进上一次提交（2026-11-12）

**现象**：`Jenkinsfile` 被误包含进 `fix(backend)` 提交（已提交未推送）。

**解决**：未推送的提交可安全重写：`git reset --soft HEAD~1`（改动退回暂存区）→ `git restore --staged Jenkinsfile`（移出暂存）→ 重新提交正确内容。已推送则不建议改历史。

### 10. Docker 拉取大层停滞（宿主机网络正常，2026-11-14）

**现象**：`docker pull node:24-alpine` 的 57MB 层零字节停滞 40+ 分钟；换 docker.1panel.live / docker.m.daocloud.io 镜像源同样停滞；但 alpine 小镜像（3MB）秒拉成功。宿主机直连与代理（龙猫云 7890）均正常、容器内 DNS 解析正常。重启 Docker Desktop 无效。

**原因**：Docker Desktop（WSL2 后端）daemon 自身的下载通道异常——小包可过、大层断流；与宿主机网络无关（排查链：宿主 curl 正常 → 容器 DNS 正常 → 小镜像可拉 → 锁定 daemon 侧）。

**解决**：给 Docker Desktop 配代理，让拉取走本地代理客户端：Settings → Resources → Proxies → Manual，HTTP/HTTPS 均填 `http://127.0.0.1:7890`（本机为龙猫云，clash 系内核默认端口；等效做法是直接改 `%APPDATA%\Docker\settings-store.json` 的 `ProxyHttpMode/OverrideProxyHttp(s)` 四键后重启 Docker Desktop）。配置后 `docker info` 可见 `HTTP Proxy: http.docker.internal:3128`，node 镜像一次拉成。

### 11. Git Bash 里 docker run 的容器路径被改写（2026-11-14）

**现象**：`docker run --rm img ls /usr/share/nginx/html` 报 `ls: D:/Git/usr/share/nginx/html: No such file or directory`。

**原因**：Git Bash 的 MSYS 路径转换把以 `/` 开头的参数当作本机路径映射到了 Git 安装目录。

**解决**：路径写双斜杠 `ls //usr/share/nginx/html`，或 `MSYS_NO_PATHCONV=1` 前缀。

### 12. 实例检测全部异常：127.0.0.1 的含义随检测执行位置变化（2026-11-14）

**现象**：go 服务容器化（第 13 天 compose 栈）后，实例页所有指向 `127.0.0.1` 的实例检测全部 `connection refused`；而同样记录在本机 `go run` 开发态检测是可达的。

**原因**：TCP 拨号发生在**运行 go 进程的位置**。本机跑时 127.0.0.1=宿主机（PG 端口映射可达）；容器跑时 127.0.0.1=brp-go 容器自身（无任何监听端口）。同一地址、两种形态、两种结果。

**解决（地址语义口径）**：compose 态下检测目标写 compose 网内服务名（如 `postgres:5432`，实测 reachable=true）或宿主机上进程用 `host.docker.internal`；`127.0.0.1` 仅本机进程开发态有效。将来真实被测 RabbitMQ 实例：LAN 上的写其 IP，实验容器组的写服务名。

### 13. compose 缺 --env-file：空变量重建容器，go 进入 fail-fast 崩溃循环（2026-11-14，M0 验收日抓出）

**现象**：`brp-go` 状态反复 Restarting（每分钟一次 Fatal：`config.yaml 缺少 auth.jwt_secret …`，RestartCount 上百），其余容器正常；`docker inspect` 见 `BRP_*` 环境变量全为空串，而 `deploy/env/.env` 里密钥明明齐全。

**原因**：platform.yml 的变量全部来自 `--env-file deploy/env/.env`。不带该参数执行 `docker compose -f … up -d` 时，`${BRP_AUTH_JWT_SECRET}` 等解析为空串；与现有容器配置不同即触发**以空 env 重建**——go 的启动 fail-fast（空密钥拒绝启动）随即崩溃循环。Taskfile 的 `up` 任务此前一直缺该参数，是长期埋着的雷。

**解决**：带 `--env-file deploy/env/.env` 重新 `up`（配置变化自动重建）；Taskfile 的 up/down 已补参数。手工执行 compose 同理必须带。防御启示：fail-fast 在此正是"哨兵"——崩溃循环看着吓人，但比空密钥静默上线好得多。

### 14. 实验栈经 Toxiproxy 连接秒断：reset by peer（2026-11-14，M1 第 16 天）

**现象**：容器网络内 pika 连 `brp_<taskId>_proxy:5672` 立即报 `IncompatibleProtocolError: Connection reset by peer`；同容器直连 `brp_<taskId>_broker:5672` 正常。

**原因**：只渲染了 `lab-template.yml`，漏渲染 `deploy/lab/toxiproxy.json`（模板 proxy 服务挂载的就是它）——upstream 仍是 `brp_<TASK_ID>_broker` 占位符主机名，toxiproxy 解析不到上游主机，接受连接后即刻断开。

**解决**：渲染必须双文件成套（yml + json 一并做占位符替换），并断言渲染产物无 `<TASK_ID>` 残留。已写入第 17 天 lab_factory 计划要点（评审建议 2）。

### 15. Windows 同端口起第二个服务不报错，端口冲突冒烟脚本卡死（2026-11-15，M1 第 18 天）

**现象**：为验证 recorder 端口占用失败路径，对已在监听的 :18081 再起一个实例——预期 bind 报错退出，实际第二实例正常运行（冒烟脚本阻塞在 serve_forever 直至超时）。

**原因**：Python `http.server.HTTPServer` 默认 `allow_reuse_address = 1`（SO_REUSEADDR）。该选项在 Linux 语义是"允许复用 TIME_WAIT 地址"（占用中的端口仍 EADDRINUSE），而 **Windows 语义是允许与正在监听的套接字双绑定**——同端口双进程都能起来。

**解决**：端口冲突（EADDRINUSE→fail-fast）路径的验证只能在 Linux/容器环境做；Windows 上写冒烟脚本凡起常驻服务必须后台化+显式 kill+超时兜底，不能假设"第二个起不来"。生产无影响（recorder 跑在 Linux 容器内，bind 冲突按预期走 `except OSError → exit 2`）。

### 16. .gitignore 行内注释不生效：/work/ 从未被忽略（2026-11-16，M1 第 21 天抓出）

**现象**：`git status` 冒出 `?? work/`——`.gitignore` 里明明写着 `/work/`。

**原因**：gitignore 的 `#` 只在**行首**（可有前导空格）才是注释起始；写在模式后面的 `#` 是模式的一部分。`/work/                  # Agent 执行工作目录…` 的实际模式是整行字符串，匹配任何路径都失败。该 bug 自仓库初建就在（day17 评审还"验证过 /work/ 覆盖"——当时目录恰好为空，掩盖了规则失效），直到 day21 网内验收残留文件落进 work/ 才暴露。

**解决**：行内注释一律拆成独立注释行；修完用 `git check-ignore -v <路径>` 逐条实证（同批修复 backend/tmp/、params.json、data/ 四条）。教训：**gitignore 规则要 check-ignore 验证，不能目测**；"目录一直是空的"不是规则生效的证据。

### 17. docker -v 挂载不存在的宿主路径：自动创建为目录，容器内文件变目录（2026-11-16，M1 第 22 天）

**现象**：`-v C:/...params-m22.json:/tmp/params.json:ro` 后容器内 pytest 全员 ERROR（params 文件读不出）；宿主机侧 `rm` 报 `Is a directory`。

**原因**：docker 对 `-v` 的宿主侧路径不存在时会**自动创建为目录**（挂载点语义）——本想挂单文件，漏了先生成文件的步骤，docker 就把该路径建成空目录挂进容器。

**解决**：挂文件前确保宿主文件已存在（生成→ls 验证→再挂）；误建目录 `rm -rf` 清掉重建。脚本化挂载时把"文件存在性检查"放在 docker run 之前。

### 18. 容器出口 TLS 全断而宿主机正常：DNS 通、握手后 UNEXPECTED_EOF（2026-11-17，M1 第 23 天）

**现象**：容器内 `pip install` 对三个 pypi 镜像源全部 `SSLError: UNEXPECTED_EOF_WHILE_READING`；同刻宿主机 curl 同源 200（但慢至 10s）。容器 DNS 解析正常、TCP 可连。

**原因**：容器出口的大包 TLS 流量被掐（典型为 MTU 分片丢弃或线路干扰；宿主路径与 docker NAT 路径受损程度不同）。Docker Desktop daemon 代理只作用于镜像拉取，**不覆盖容器内 egress**。

**解决**：验收编排解围法（零容器网络）：宿主 `uv pip install --python 3.13 --target <dir> docker pytest`（uv 走宿主网络）→ `-v <dir>:/pylibs:ro -e PYTHONPATH=/repo:/pylibs` 挂进 pytest 容器 → `--entrypoint python … -m pytest`（pylibs 的 bin 不在 PATH，勿用 pytest 直做 entrypoint）。长期解：测试编排依赖烘进 brp-lab:dev 镜像（已列 lab/requirements.txt，重建待网络恢复）。教训：**验收链路里凡"容器内临时 pip install"都是单点网络依赖**，要么烘焙要么挂载。

### 19. Windows 下 bash 的 /tmp 与原生 Python 的 /tmp 不是同一个地方（2026-11-18，M1 第 26 天）

**现象**：Git Bash 里 `python -c "open('/tmp/x','w')"` 写文件后，`docker run -v "C:/Users/<u>/AppData/Local/Temp/x:…"` 挂载说文件不存在——docker 把挂载点建成目录（#17 连锁），pytest 全员 ERROR；同脚本的 bash `printf > /tmp/x` 却一直好使。

**原因**：Git Bash（MSYS）把 `/tmp` 映射到 `C:\Users\<u>\AppData\Local\Temp`，但**原生 Windows Python 不经 MSYS**——`open('/tmp/x')` 相对当前盘符解析为 `F:\tmp\x`（本仓库在 F: 盘）。同一个 `/tmp` 字面量两个世界。

**解决**：跨 bash/Python 的临时文件一律让 Python 用 `os.environ['TEMP']` 定位（或写死 `C:/Users/<u>/AppData/Local/Temp`），勿在 Python 里写 `/tmp/...`；顺带两条循环纪律：`cmd | tail` 后 `$?` 是 tail 的退出码（取真实 rc 用输出落文件或 `PIPESTATUS[0]`）；挂载前 `ls -la | grep "^-"` 预检文件真的是文件。

## GORM 与 ORM

### 20. GORM tag `type:int` 被内置常量名吞掉，列落成 bigint（2026-11-20）

**现象**：`Attempt int` 想按表 20 原文落 `int` 列，加了 `gorm:"type:int"` 后 AutoMigrate 生成的仍是 bigint。

**原因**：GORM schema 解析把 `type:` 值赋给 `field.DataType`，而 `"int"` 恰好等于内置常量 `schema.Int`——命中 postgres driver 的 Int 分支后走默认 size 逻辑（int→bigint），type 写法被短路。

**解决**：类型名写 PG 原生名 `type:integer`（`"integer"` 不与任何 GORM 常量冲突）。教训：GORM 的 type tag 要写目标数据库方言的原生类型名，不写 Go 类型名。

### 21. AutoMigrate 只加不改：已有列的类型不会被 ALTER（2026-11-20）

**现象**：修正模型 tag（bigint→integer）后重启服务，列类型依旧是 bigint，且迁移无任何报错。

**原因**：GORM AutoMigrate 只创建缺失的表/列/索引，不比较也不修改已有列的类型——静默跳过。

**解决**：表还在零行阶段直接 `DROP TABLE` 后重启由 AutoMigrate 重建（无损）；已有数据的表必须手写 ALTER 迁移，AutoMigrate 指望不上。

### 22. 查询结果集嵌套 struct 被 GORM 当关联解析（2026-11-21）

**现象**：`type execView struct { Exec model.TestExecution; TaskStatus string }` 用 `Find(&views)` 扫 JOIN 结果报错：`invalid field found … define a valid foreign key for relations`。

**原因**：GORM 见到结构体字段是另一个 Model 类型，默认按关联（association）处理，试图找外键定义——而这里只是想平铺列。

**解决**：加 `gorm:"embedded"` tag（Exec 列平铺进视图）；别名列配 `gorm:"column:task_status"`。JOIN 视图/DTO 场景记住 embedded。

## Go 依赖库

### 23. jsonschema/v6 的 AddResource 收解码后的 JSON 值，不收 io.Reader（2026-11-20）

**现象**：`c.AddResource(name, bytes.NewReader(raw))` 编译通过，Compile 时报 metaschema 校验失败 `invalid jsonType *bytes.Reader`。

**原因**：v6 的 AddResource 参数是解码后的 JSON 值（`any`）——传 Reader 会被当作 JSON 值本身去校验。v5/v1 传 Reader 的老习惯不适用。

**解决**：先 `jsonschema.UnmarshalJSON(reader)` 解出值再传入（顺带保留 json.Number，与校验侧 UseNumber 口径一致）。另注意：v6 对 schema 的 `format` 默认不校验（no-op），date-time 这类格式断言要么注册 format 回调、要么像本项目一样在应用层 `time.Parse(RFC3339)` 等价后置。

### 24. go:embed 按指令声明匹配：新增文件不进旧指令（2026-11-21）

**现象**：目录里已有 `//go:embed schema/params_snapshot.schema.json`，新拷入 `schema/events.schema.json` 后运行时 `schemaFS.ReadFile` 报 file does not exist。

**原因**：embed 指令显式列出的是**单个文件**，不隐含目录其余文件；embed 没有目录级默认。

**解决**：改为模式匹配 `//go:embed schema/*.schema.json`（目录内全部 schema 一网打尽，新增零维护）。

## 并发与事务

### 25. check-then-act 注册无锁：并发同键注册分裂多行（2026-11-21）

**现象**：register 走"先 FindByHostname 查、无则 Create"两步，功能测试全绿；评审 12 并发 HTTP 探针同 hostname 注册，落出 3 行 3 个 agent_id——分裂行的 token 永不失效成悬挂凭证。本地 30 goroutine 重试多次未复现：**竞态存在与否不以踩没踩中为准，代码层 check-then-act 无锁即成立**。

**原因**：查与插之间存在窗口，唯一性只靠应用层判断，无数据库约束兜底。

**解决**：`hostname` 加唯一约束 + 单语句 `INSERT … ON CONFLICT (hostname) DO UPDATE … RETURNING agent_id`（原子 upsert，冲突分支即"复用身份换凭证"）。教训：幂等不变量必须落到数据库约束上，且并发测试钉不住≠安全（时序运气）。

### 26. 并发测试钉不住 SKIP LOCKED：阻塞式 FOR UPDATE 结果相同（2026-11-20）

**现象**："10 并发抢 1 行恰 1 成功"的集成测试全绿，但把 `Options:"SKIP LOCKED"` 删掉测试照样绿——READ COMMITTED 下后来者等锁释放后发现行已不匹配，同样返回空。测试证明了原子性，没证明 SKIP LOCKED 特性本身。

**解决**：行为级钉子：会话 A 开事务 `FOR UPDATE` 锁住唯一 PENDING 行**不提交**；会话 B 调 claim 必须 2s 内跳过该行返回"无任务"——退化成阻塞锁时 B 挂在行锁上超时判红。双向验证过（删 Options 红/恢复绿）。教训：特性测试要构造"只有该特性才能通过"的场景，而不是结果等价场景。

## 测试编写

### 27. fake 的固定时钟与真实时钟混用：隔天测试全体爆雷（2026-11-21）

**现象**：day33 心跳 fake 用固定 `testNow=2026-11-21 12:00 UTC` 构造租约 `testNow+1h`，被测代码用 `time.Now()`——当天下午真实时钟越过 13:00 UTC 后 fake 判定"租约已过期"，全套心跳测试无端变红。

**原因**：fixture 的时间基准（固定值）与被测路径的时间基准（真实时钟）不一致，固定值提供的是**有效期下限**而非可控时钟。

**解决**：fixture 改 `time.Now().Add(time.Hour)`（相对基准）。教训：fake 时间要么纯注入（被测代码也收 now 参数）、要么纯相对（一律 time.Now() 推导），绝不对半混用。

### 28. channel 断言两坑：closed 恒可读 + len 动态递减（2026-11-21）

**现象①**：订阅 cancel 后 `select { case <-ch: t.Fatal("不应投递") }` 必然触发 Fatal；**现象②**：`for i := 0; i < len(ch); i++ { <-ch }` 消费到一半提前退出，漏收后半（CANCELLING 广播断言因此假失败）。

**原因**：① closed channel 的接收**恒立即可行**（返回零值 ok=false），select 无法区分"有投递"与"已关闭"；② `len(ch)` 随每次接收递减，动态上限每轮都变小。

**解决**：① comma-ok 断言 `case ev, ok := <-ch: if ok { Fatal }`；② 先快照 `n := len(ch)` 再按 n 消费。

## 脚本与工具（续）

### 29. 脚本中途崩溃残留子进程：双订阅者交错污染输出文件（2026-11-21）

**现象**：验收脚本（curl -N 订阅 SSE 落文件）中途异常退出，重跑后输出文件里每条事件出现两遍、"重放恰一次"断言失真。

**原因**：崩溃路径没走到 terminate，旧 curl 进程仍持着订阅与文件句柄继续写；重跑又起一个——两个订阅者各收一份、写同一个文件。

**解决**：脚本开头 `taskkill /F /IM curl.exe` 清残留 + 子进程消费放 try/finally（terminate+wait，异常也 kill）。教训：凡脚本起常驻子进程（订阅/服务），必须保证"任意退出路径都清理"——崩溃防御要在入口做，不能只靠正常收尾。

### 30. Git Bash 管道写中文文件编码混编；GBK 控制台打印崩（2026-11-20~21）

**现象**：bash 里 `cmd | tee file` 或 `xargs … | sort | uniq` 产出的文件中文段落混编损坏（评审读档发现）；Python 脚本 print 含 ✓ 等字符在 GBK 控制台直接 UnicodeEncodeError 崩溃（文件本身无损）。同族：Git Bash 的 `/tmp` 与原生 Python 的 `/tmp` 不是同一目录（#19）在 tee+python 组合里再度复发。

**原因**：Git Bash 管道默认按 locale（GBK/cp936）转码混入；Windows 控制台代码页非 UTF-8 时 stdout 编码炸。

**解决**：验收/证据类文件**一律由 Python 脚本整体生成**（`open(path,"w",encoding="utf-8")`），bash 只负责起服务；Python 脚本头部 `sys.stdout.reconfigure(encoding="utf-8")`；bash 与 Python 之间不传文件路径内容（各管各的临时目录）。教训与 #19 同源：跨 bash/Python 边界的东西（路径/编码）全部显式声明，不靠默认。

### 31. 集成测试设施与生产配置漂移：TranslateError 单侧生效 + TRUNCATE 清单不随新表更新（2026-11-22）

**现象**：①uq 并发兜底集成测试收到裸 23505 PgError 而非 ErrDuplicatedKey——生产 OpenPG 已开 `TranslateError: true`，测试仍穿透；修完①后并发用例又"两发全撞唯一键"——test_result 表残留上一轮行。

**原因**：①集成测试用独立的 `gorm.Open`（openIntegrationDB）只配了 Logger，**没抄生产的 TranslateError**——同一份代码两种错误形态，测试结论失真（会误判生产缺陷或掩盖真缺陷）；②openIntegrationDB 的 TRUNCATE 清单是 day31 写死的四表，day35 新增三表没进清单——跨轮残留行让并发断言错乱。

**解决**：①集成设施配置与生产逐项对齐（TranslateError 等）；②TRUNCATE 一律列全清单或改 `TRUNCATE ... CASCADE` + 显式列维护惯例。教训：**自建测试设施的配置是一份"影子生产配置"，每次改生产 OpenPG 必须同步检查测试侧**；TRUNCATE 清单跟着表清单走（加表日即加清单日）。

### 32. 决策表重构的短路陷阱："拒绝"被写成 return，旁路动作被跳过（2026-11-23）

**现象**：events 端点接线状态机决策表后，"RUNNING 态收 STATUS 不转移但仍更新执行子状态"的既有用例变红——子状态不再更新。

**原因**：转移判定写成了 `if !tj.OK { return nil }`——决策表对 (RUNNING, first_status) 返回拒绝，但该分支的正确语义是"**不转移、继续往下走**"（子状态更新是转移之外的旁路动作）；return 把旁路动作一起短路了。重构时把"转移被拒"与"整个请求处理终止"两个层次混为一谈。

**解决**：拒绝仅跳过转移块：`if tj := transition(...); tj.OK && tj.To != "" { 转移+广播 }`，子状态更新留在 if 外。教训：**引入决策表/规则引擎时，先分清"拒绝"的辖域**——它拒绝的是这一件事，不是整个请求；旁路副作用的位置在接线前要逐项清点（本次由既有回归网抓住，未流出）。
