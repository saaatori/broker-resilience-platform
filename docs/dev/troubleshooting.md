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

### 33. Windows 上 python3 是应用商店 stub：静默零输出退出（2026-11-23）

**现象**：`python3 - << 'EOF' … EOF` 的补丁脚本看起来"跑了"（命令链继续、exit 0），但目标文件没改、脚本内 print 无输出；同样的脚本用 `python` 执行完全正常。

**原因**：Windows 无内置 python3 别名——`python3` 命中 Microsoft Store 的 App Execution Alias stub，无参数/管道输入时直接静默退出（exit 0），不执行任何代码。此前所有 heredoc 补丁都用 `python`，误敲一次 `python3` 即触发。

**解决**：本机一律用 `python`（独立安装 3.13）。防再犯：heredoc 补丁脚本必须带一行 `print("patched")` 尾输出——输出缺失即说明脚本根本没执行（早于文件级 assert 发现问题）。

## 忽略规则与测试资产

### 34. .gitignore 通配符误伤测试夹具：本机全绿、新克隆缺文件（2026-11-24）

**现象**：本机 agent 套件全绿；把 `HEAD` 导出成全新克隆再跑，`test_fixture_file_counts` 报 `FileNotFoundError: …agent/tests/fixtures/reconcile_sample.jsonl`。

**原因**：`*.jsonl` 本意忽略运行产物（`/work/` 下的 JSONL），但对账单测的夹具同后缀，一并被忽略——文件只存在于开发机，从未入库。#16 的教训当时只验证了"该忽略的被忽略"，没验证"不该忽略的没被误伤"。

**解决**：通配符之后加否定规则 `!agent/tests/fixtures/*.jsonl`；用 `git ls-files --others --exclude-standard` 确认夹具出现在未跟踪列表（`git check-ignore -v` 应命中否定规则）。"新克隆会怎样"不要靠想象：`git archive HEAD | tar -x -C <临时目录>` 实跑一遍。教训：**每加一条通配忽略规则都要双向核对**——产物被忽略，夹具/样例未被误伤。

## 容器探针

### 35. healthcheck 只发请求不读响应＝端口探活，假绿（2026-11-24）

**现象**：recorder 的 healthcheck 恒为 healthy，但 recorder 只实现了 `POST /records`——`GET /healthz` 实际返回 501。

**原因**：探针是 `http.client.HTTPConnection(...).request('GET', '/healthz')`，只调 `request()` 不调 `getresponse()`；进程退出码只取决于"连得上、发得出"，端口通即绿，服务端怎么答根本没看。

**解决**：实现 `GET /healthz`；探针改 `sys.exit(0 if conn.getresponse().status == 200 else 1)`。验证用三组对照：新代码+新探针=healthy；旧代码+新探针=连续 exit 1；旧代码+**旧探针**=healthy（假绿实证）。教训：**探针必须断言响应**（状态码或响应体），"只发不读"的 healthcheck 没有证明力。

## 验证脚本

### 36. 子进程 stdout 接了管道却不读：写满后子进程被阻塞（2026-11-24）

**现象**：验证脚本里服务"起不来"（healthz 永远不通），手工单独跑同一个二进制却完全正常。

**原因**：`subprocess.Popen(stdout=PIPE)` 之后没人读管道；启动迁移期日志量大，管道缓冲写满后子进程阻塞在 `write` 上，永远走不到监听。

**解决**：子进程输出落文件（`stdout=open(log, "w", encoding="utf-8")`），或起线程持续读管道。"手工能跑、脚本不行"先怀疑编排，而不是被测程序。

### 37. httptest.Server 自定义超时的正确写法（2026-11-24）

**现象**：`s := &httptest.Server{Config: srv}; s.Start()` 后 `s.URL` 是空的，请求报 `unsupported protocol scheme ""`。

**原因**：零值构造的 `httptest.Server` 没带监听器，不是文档支持的用法。

**解决**：走文档写法 `ts := httptest.NewUnstartedServer(handler)` → 改 `ts.Config` 的超时字段 → `ts.Start()`。要测真实构造器的字段映射，就把构造器返回值的各字段赋给 `ts.Config`。

### 38. 服务端提前拒绝超大请求时，客户端可能先看到"连接被中止"而非 413（2026-11-24）

**现象**：5MB 请求体触发上限后，Python urllib 抛 `ConnectionAbortedError`（WinError 10053），读不到 413 报文；同样的请求 curl 得到 413。

**原因**：服务端见 `Content-Length` 超限即回 413 并关闭连接（刻意不读完任意大体积的 body），此时客户端还在上传，连接复位先于它读到响应。

**解决**：这是拒绝生效的正常表现。验证"是否被拒"要看副作用（如任务数未增加），并用另一类客户端复测；不要因为一个客户端抛的是异常就判定上限没生效，也不要为了给客户端一个干净报文而把超限 body 读完。

## 测试命令与环境

### 39. 任务里的裸 pytest 解析到全局 Python（2026-11-24）

**现象**：`task test` 在 agent 套件收集阶段就失败：`tests/test_lab_factory.py` 里 `from docker.errors import NotFound` 报错；而 `uv run pytest` 正常。

**原因**：`test:agent` 用的是裸 `pytest`，在没激活 venv 的终端里命中全局 Python 的 pytest（#5 双 Python 并存的同族问题，缺 `docker` 包）。一直没发现，是因为日常只跑 `test:backend`，或在已激活 venv 的终端里跑。

**解决**：任务改 `uv run pytest tests`（走项目环境）；新增的 lab 任务同口径 `uv run --project agent pytest`。教训：**进任务文件和 CI 的命令不能依赖"终端恰好激活了哪个环境"**——写完任务换一个干净终端跑一遍。

### 40. 抄上一行断言忘了翻转条件（2026-11-24）

**现象**：限流器"窗口过期后应解除"用例失败；把变量值打出来，实现却完全正确。

**原因**：前一行"前置：应处于限流"写的是 `if !blocked { Fatal }`，被原样抄到期望恰好相反的下一行。

**解决**：先把关键值打出来，确认实现在做什么，再回头审断言——这次 30 秒的打印省下了对正确实现的"修复"。相邻两条断言期望相反时，条件必须显式翻转。

## 并发与事务（续）

### 41. 限流器"判定"与"记账"分两步：并发可整体绕过（2026-11-24）

**现象**：登录限流的串行测试全对（5 次错密后第 6 次 429）；但 40 个并发错密得到 401×40、429×0——上限明明是 5。

**原因**：处理器先调 `blocked()` 判定，校验完口令才调 `recordFailure()` 记失败。并发请求在任何人记下失败之前就全部通过了判定。与 #25（注册先查后插）是同一个错误：把"检查"和"动作"分成两步，中间留着窗口。写的时候明知 #25，还是写了。

**解决**：把"判定 + 预占额度"合成一次加锁内的原子操作（`admit`），口令校验完再结算（`settle`）；在途（已预占、未结算）的尝试本身占额度。第一次改完并发测试仍是红的——拒绝条件只算了已失败数，漏了在途数（注释写了、代码没写），单元测试和并发测试同时抓到。

教训：①**凡是"计数 / 限额 / 幂等"逻辑，写完先写并发测试**，不要只靠串行用例；②给假仓库加一点查询延迟，把竞态窗口拉宽，让它在测试里确定性地暴露，而不是靠时序运气；③注释不是实现——改完以测试为准，不以"我觉得写对了"为准。

## 依赖与锁文件

### 42. uv.lock 把镜像站地址写死；换源同步会整体改写锁并重新解析版本（2026-11-24）

**现象**：全局 uv 配置指向清华镜像；镜像返回 403 后，空缓存装不出环境（换到家庭网络仍是 403，与出口 IP 无关）。临时加 `--default-index https://pypi.org/simple` 能装上，但锁文件被整体改写——清华地址 302 处全部变成 pythonhosted，且有 63 行包名/版本与原锁不同。

**原因**：uv 把每个包的来源 registry 和安装包 URL 都记进 `uv.lock`。默认索引一变，uv 认为现有锁与索引不匹配，于是重新解析全部依赖，而不是只替换地址——版本因此可能升到更新的发布版。锁文件进了仓库，等于仓库的可复现性绑定了那个镜像站。

**解决**：①镜像恢复后回到原配置重新 `uv sync`，保持锁的来源不变；②镜像不可用时，不要把换源后生成的锁直接提交——要么等恢复，要么明确接受"升级依赖基线"并全量回归；③不想被依赖问题卡住的功能优先用标准库（本项目的 recorder、setup 脚本都是纯标准库的先例）。诊断类实验一律在临时目录的副本上做。教训：**换源不是无害操作**——提交前先看 `git diff uv.lock` 的规模。

## 端口、镜像与验收探针

### 43. 回环端口被原生服务占着：容器绑 127.0.0.1 起不来，`localhost` 与 `127.0.0.1` 连到的不是同一个 Redis（2026-11-24）

**现象**：把 compose 里 redis 的映射从 `"6379:6379"` 收紧成 `"127.0.0.1:6379:6379"` 后，`up -d` 报 `listen tcp4 127.0.0.1:6379: bind: Only one usage of each socket address...`，redis 以及排在它后面的 go / agent / frontend 全部没起来（postgres 先重建成功，栈停在半启动）。此前 `0.0.0.0:6379` 一直能起。

**原因**：本机装着禅道（`F:\Chan_Dao\ZenTao\zbox`），它自带的 Redis 以 Windows 服务 `rediszt3`（nssm 托管、开机自启）运行，监听 `127.0.0.1:6379`。Windows 允许"通配地址 + 特定地址"两个套接字绑同一个端口，所以容器的 `0.0.0.0` 映射一直与它"共存"；改成特定地址 `127.0.0.1` 就正面冲突。共存期间连接的去向：显式 `127.0.0.1:6379` 命中原生 Redis（项目口令 `AUTH` 得 WRONGPASS），`localhost` 先解析到 IPv6 回环时经 `[::]` 命中容器（`INFO server` 显示 `os:Linux`）——本地开发能用，靠的是地址族的解析顺序。

**解决**：redis 映射退回 `"6379:6379"`（注释写明原因），postgres 与 80 维持回环。定位方法：`netstat -ano | grep :6379` 拿 PID，`tasklist /FI "PID eq <pid>"` 看是谁。要真正收紧二选一：停用禅道的 Redis 服务，或把容器映射换到不冲突的回环端口并同步本地 `redis.addr` 与集成测试里的常量。**后续（2026-11-25）**：禅道服务停用并改为手动启动后，已收紧为 `127.0.0.1:6379:6379`，`127.0.0.1` 与 `localhost` 连到的都是容器。附带一个新现象：`[::1]:6379` 不再监听——只绑 IPv4 回环时 Docker 不监听 IPv6 回环（80 / 5432 相同）。写 `localhost` 的客户端会回退到 `127.0.0.1`（Go、Python 实测可以），显式写 `::1` 的客户端连不上，改用 `127.0.0.1`。教训：**改端口绑定之前，先查该回环端口上有没有原生监听**；"端口映射起来了"不等于"`localhost` 连的就是容器"。

### 44. `task up` 不重建镜像：容器里跑的是十天前的后端（2026-11-24）

**现象**：从 Agent 容器直连 `go:8080` 调 `/api/v1/agent/heartbeat`，得到的是 `404 page not found`（Go 的默认 404，不是鉴权的 401）；经 :80 入口建任务同样 404。本机 `task dev:backend` 一切正常。

**原因**：`task up` 等于 `docker compose ... up -d`，不带 `--build`。镜像只在首次构建时生成，之后代码改多少次，`brp-backend:dev` 都停在第一次——本机那份是 11-13（第 13 天）构建的，任务与 Agent 路由都是 11-20 之后才加的。此前各天的验收都走宿主机的 air 热重载，所以没人发现 compose 里的后端早已过期。

**解决**：`docker compose -f deploy/compose/platform.yml --env-file deploy/env/.env up -d --build go` 重建（`go mod download` 的缓存层失效时需要联网），重建后的容器日志里能看到 Agent 路由注册。`task up` 是否默认带 `--build`、或另加一个 `task rebuild`，待定。教训：**做 compose 验收之前先看镜像的构建时间**（`docker image inspect <image> --format {{.Created}}`）；"容器健康"只说明进程活着，不说明代码是新的。

### 45. 验收探针经 claim 领任务：领到的是库里更早的 PENDING 任务（2026-11-24）

**现象**：黑盒脚本建了两个任务（id 5、6）准备让探针 Agent 领取并回传，实际两次 claim 返回的是 id 1 和 3——当天另一个会话留下的 PENDING 任务。脚本把失败结果回传到了这两条别人的任务上，而清理只按自建的 id 删，现场没有复原。

**原因**：claim 取的是"最早的 PENDING"，不认是谁建的、什么时候建的。探针默认"领到的就是我刚建的"，这个前提在开发库有残留时不成立。

**解决**：①探针加两道保护：建任务**之前**先查 `test_task` 里有没有更早的 PENDING，有就停、什么都不建；每次领取之后核对 `task_id` 是否在自建清单内，不在就停止回传。②现场按"确认是自己造的行"还原：核对执行记录的 `started_at` 之后，删除对应的 `test_execution` / `test_result` / `test_metric_summary`，把任务状态改回 PENDING；清理语句一律同时带 id 与时间条件。教训：**会消耗共享资源的验收脚本，先假设库里有别人的东西**；清理按"实际动过的对象"（领取返回的 execution_id）来做，而不是按"自己建的输入"。

### 46. 三类假断言：nginx 覆盖 Server 头、jsonb 读回是规范化文本、只有负例的测试（2026-11-24）

**现象**：①探针用响应头 `Server:` 是否带上游特征来判断"请求有没有到达上游"，4 条透传用例全红，而状态码明明是上游的 501 / 404。②集成测试用字面量 `"reason":"timeout"` 比对 `test_result.failure` 读回的文本，断言失败，数据其实已经落库。③一条只含负例的新测试，在功能还不存在的旧 schema 上也通过。

**原因**：①nginx 默认用自身的 `Server` 头覆盖被代理响应的头，从头部看不出上游是谁。②jsonb 列读回的是 PostgreSQL 的规范化文本（冒号后带空格、键有序），与写入时的字面不一致。③旧 schema 因为"未知键"报错，恰好让"应当被拒收"的断言成立。

**解决**：①判断是否透传，看**响应体**里的上游特征（如假上游的 `Unsupported method`）；判断是否被 nginx 拦截，看它默认 404 页里的 `<hr><center>nginx`——注意它带版本号，标记不能写成闭合标签。②jsonb 列按内容断言：解码之后比字段。③负例测试的开头加一条基线断言，未改动的正例必须通过。教训：**断言只依赖行为契约里的东西**（状态码、响应体字段、解码后的值），不依赖传输层会改写、或存储层会规范化的形态。

### 47. fake 仓库省掉了真仓库的更新条件：一类行为在单测里永远测不到（2026-11-24）

**现象**：评审指出，按协议自己的取消时序（STATUS 上报子状态 ABORTED → 回传 result），`exit_reason` 不会落库。写集成测试一跑就红（执行记录 `ABORTED / <null> / <null>`）。但此前所有单测都是绿的；事后回看，"已收口的执行不被覆盖"这条行为也没有任何测试真正钉住。

**原因**：两件事叠加。①真仓库 `FinishExecution` 的更新条件写的是"状态不是终态"，而 STATUS 事件会把 DONE / ABORTED 写进状态——终态子状态先到时，result 的收口就成了 0 行更新。②fake 仓库的同名方法是无条件更新，根本没有这个条件。于是真仓库的行为在单测里不可见：既暴露不了这个缺陷，也证明不了"终态保持"。

**解决**：①真仓库的条件改成按"是否已收口"判定（`finished_at IS NULL`）——先收口者固化。清扫器只处理非终态执行，对它两种条件等价；只有"终态子状态先到、结果后到"这一种情况改变行为。②fake 补上同一个条件（空退出原因也不再写成空串）；依赖它的既有造数补齐 `finished_at`（原来只给了 DONE）。③两条集成测试钉住：终态子状态先到仍须收口；已收口不被覆盖。教训：**fake 与真仓库的差异不只在"有哪些表"，还在"哪条语句带什么条件"**——真仓库里带 WHERE 条件的写操作，fake 要么复刻条件，要么明确记档"此处不保真"，并有一条集成测试兜住。与 #31（测试连接漏配 TranslateError）同源。

## 测试环境与开发环境的分隔

### 48. 集成测试对着开发库跑：夹具场景和垃圾任务留在开发库里（2026-11-25）

**现象**：开发库里多出两个场景 `sweep_scan_fin`、`sweep_scan_int` 和四个挂在它们上面的任务，其中两个停在 PENDING。这是集成测试夹具造的数据——Agent 一上线会先领走这两个任务，而它们的场景名在 `lab/scenarios/` 下没有对应目录，参数快照也缺协议要求的键。

**原因**：三层问题叠在一起。①集成测试的辅助函数开头会 `TRUNCATE` 业务表并写入夹具数据，跑在哪个库就改哪个库；②它靠 `BRP_TEST_DSN` 指定目标库，而 `BRP_TEST_DSN` 未设置时的跳过提示给出的示例就是**开发库的连接串**，测试 Redis 更是写死了 `localhost:6379` 加开发口令——代码本身在把人往开发环境上引；③跑隔离环境要手工起容器、等就绪、设变量、跑完再删，步骤一多就有人图省事直接用开发库。只写一条"不要对着开发库跑"的规则挡不住这个。

**解决**：规则、工具、护栏三层一起做。①`task test:integration` 一条命令完成"起一次性容器 → 等就绪 → 设 `BRP_TEST_*` → 跑 → 删"，测试失败也删，就绪等待放在容器内做（不依赖宿主机的 `sleep` / `seq`，PowerShell 下也能跑）；②测试代码只认 `BRP_TEST_DSN`、`BRP_TEST_REDIS_ADDR`、`BRP_TEST_REDIS_PASSWORD`，不回退到开发环境；③护栏：库名不是 `brp_test` 开头直接拒绝运行，且排在任何写入之前（对着一个名叫 `broker_resilience` 的空库实测：拒绝前后都是 0 张表）；配置只配一半（有 DSN 没 Redis 地址）或声明的 Redis 连不上时**报错而不是跳过**，避免依赖 Redis 的用例静默消失、测试结论看起来全绿。教训：**分隔不能只靠自觉，要让"省事的路"就是"安全的路"**；测试基础设施里的默认值和提示语也是代码，写错了同样会把人带沟里。

## 依赖与锁文件（续）

### 49. 锁文件绑定的镜像站返回 403：镜像里 `uv sync --frozen` 装不出依赖（2026-11-26）

**现象**：Agent 镜像第一次真正装依赖（此前是零依赖占位），Dockerfile 里 `uv sync --frozen --no-dev` 在构建容器中失败：下载 `charset-normalizer` 的 wheel 时 `https://pypi.tuna.tsinghua.edu.cn/packages/...` 返回 `403 Forbidden`。在宿主机上用 curl 请求同一个地址，UA 换成 `uv/0.12.17`、`pip/26.0`、`curl/8.0`、`python-urllib/3.13` 都是 403——不是客户端被识别，是这个镜像站对这台机器整体不可用。`/simple/` 索引页同样 403；同属 TUNA 的 bfsu 镜像也是 403。

**原因**：三层叠在一起。①`uv.lock` 记的是每个包的来源 registry 和 wheel 的下载地址（#42），`--frozen` 就是按这些地址原样下载，构建时改不了指向；②不冻结、改默认索引去同步，uv 会重新解析并整体改写锁文件（#42 的教训），而锁文件在仓库里；③本机的 uv 缓存里已有这些 wheel，所以宿主机上离线同步一切正常，问题只在"空缓存的构建容器"里出现。

**解决**：不动锁文件，把"解析"和"下载"拆开。Dockerfile 分两段：导出阶段用 uv 离线从锁文件导出"精确版本 + sha256"清单（`uv export --frozen --offline --no-dev --no-emit-project`），运行阶段用 `pip install --require-hashes` 安装。哈希校验保证换一个站拿到的仍是锁定的同一批文件；安装源做成构建参数 `PIP_INDEX_URL`（默认 `https://pypi.org/simple`），网络慢时 `--build-arg` 换镜像站。当天从构建容器内实测：pypi.org、腾讯云、阿里云返回 200，清华、华为云返回 403——华为云在宿主机上是 200。教训：**锁文件把可复现性绑在了一个镜像站上，而镜像站的可用性不归你管**；需要"按别的站下载同一批文件"时，导出带哈希的清单比改锁稳；**选源要在构建容器那一侧测**，宿主机通不代表容器通，反过来也一样。

### 50. 官方 uv 镜像没有 shell：`RUN` 的 shell 形式起不来，`uv` 也不在 PATH 上（2026-11-26）

**现象**：导出阶段 `FROM ghcr.io/astral-sh/uv:0.12.17`，`RUN uv export ...` 报 `exec: "/bin/sh": stat /bin/sh: no such file or directory`；改成 exec 形式 `RUN ["uv", ...]` 后又报 `exec: "uv": executable file not found in $PATH`。

**原因**：不带后缀的 `ghcr.io/astral-sh/uv:<版本>` 是 distroless 镜像，只有 `/uv`、`/uvx` 两个可执行文件，没有 shell，`/uv` 也不在 `$PATH` 里——它靠 ENTRYPOINT 调用，而 `RUN` 不经过 ENTRYPOINT。

**解决**：`RUN ["/uv", "export", ...]`——exec 形式加绝对路径。只想借 uv 解析锁文件时，这样就不必换用带 shell 的 `-bookworm-slim` 变体。若要把 uv 带进自己的基础镜像，`COPY --from=ghcr.io/astral-sh/uv:<版本> /uv /uvx /bin/` 也可以，但它会成为独立的一层，之后再 `rm` 镜像也不会变小（当天单阶段版 280MB，改成两段式、uv 不进运行阶段后 201MB）。

## 忽略规则与测试资产（续）

### 51. `.dockerignore` 不带斜杠的模式只匹配上下文根这一层：`__pycache__/` 挡不住子目录（2026-11-26）

**现象**：`.dockerignore` 里写了 `__pycache__/` 和 `*.pyc`，构建出的镜像里 `executor/__pycache__/` 仍然在——是宿主机上 pytest 跑出来的缓存，带着本机的修改时间一起被 `COPY` 进去了。

**原因**：`.dockerignore` 的模式按 Go 的 `filepath.Match` 规则匹配，不带 `/` 的模式只对上下文根目录下的名字生效，不递归到子目录。`tests/`、`.venv/` 这类在根上的目录挡得住，`executor/__pycache__` 挡不住。

**解决**：子目录用 `**/` 前缀：`**/__pycache__`、`**/*.pyc`。Agent 的 `.dockerignore` 已改；`lab/.dockerignore` 是同一种写法，而 `lab/Dockerfile` 是整目录 `COPY . /app/lab`——当天查了现有的 `brp-lab:dev` 镜像，`/app/lab/helpers/__pycache__` 与 `/app/lab/scenarios/normal_delivery/__pycache__` 两个目录确实在里面（无害，但不干净，下次重建 lab 镜像时一并改）。教训：**忽略规则写完要看一眼镜像里实际有什么**（`docker run --rm <镜像> ls -A <目录>`），"配了"不等于"生效了"。

## 端口、镜像与验收探针（续）

### 52. 只 `up` 一个服务，没改动的依赖容器也被重建；事后再查，证据已经没了（2026-11-26）

**现象**：改了 compose 里 agent 服务的配置后执行 `docker compose ... up -d --build agent`，预期只重建 agent，实际 postgres、redis、go、agent 四个容器都被重建（frontend 没有）。事后想查原因：旧容器已销毁，`docker events` 里只剩最近几分钟的健康检查事件。

**原因**（查到的部分）：

- **go**：`--build <服务>` 会把被选中的服务里所有带 `build:` 的一并构建——`agent` 依赖 `go`，`go` 也被选中并重新构建。重建后的镜像内容没变（`Created` 仍是两天前），但镜像摘要变了，compose 据此判定"镜像已更新"而重建容器。
- **postgres / redis**：**未查明**。在一个一次性的 compose 项目里复现（a ← b ← c ← d，只改 c 再 `up -d --build c`），结果只有 c 被重建，依赖与被依赖者都没动——说明 compose 没有"连带重建"的规则。那么当时这两个服务的配置或镜像相对运行中的容器应当本来就有差异，但无法取证。
- **事件缓冲**：`docker events` 只返回了 256 条，正好是 daemon 内存缓冲的上限。三个带健康检查的容器每 5 秒各产生 3 条 exec 事件，几分钟就把更早的记录冲掉了。

**解决**：①动栈之前先 `docker compose ... up -d --dry-run [<服务>]` 预演，它会原样打印将要 Recreate / Running 的容器；向用户说明"会动哪些容器"时照 dry-run 的结果讲，不要按"我只指定了一个服务"推断。②重建之后核对数据面：数据卷是保留的，但要用 `.env` 里的口令实际连一次 PG 和 Redis、数一遍行数，并经 :80 入口确认 nginx 到 go 的反代仍然通。③可能需要事后取证时，动栈之前先把各容器的标签存下来（`docker inspect <容器> --format '{{json .Config.Labels}}'`，其中 `com.docker.compose.config-hash` 就是 compose 判断"配置是否变过"的依据）。④只想更新一个服务时，别用 `up -d --build <服务>`：它会把被选中服务里带 `build:` 的依赖镜像一并重建，依赖容器随之被重建（go 那次就是这样）。分成两步——`docker compose … build <服务>` 只构建这一个镜像，再 `up -d <服务>`（不带 `--build`）。当天第二次更新 agent 就是这么做的，预演与实际都只有 agent 被重建。另记：容器的 `com.docker.compose.depends_on` 标签格式是"依赖:条件:restart"，第三段是 depends_on 的 `restart` 选项（默认 false），不能拿它判断启动依赖条件是否满足——条件是否生效，看 `up` 输出里的等待顺序和容器启动日志。教训：**"我只动了 X"是对输入的描述，不是对结果的描述**；结果要看 dry-run 和事后核对。另记一条相关的实测：compose 的配置哈希不含 `depends_on`——只改启动依赖关系时，运行中的容器不会被重建，新的依赖条件要到下次重建才生效。

## 测试编写（续）

### 53. 把"关掉监听套接字"当成"服务不可达"：keep-alive 连接还活着（2026-11-26）

**现象**：测试里先让 HTTP 客户端对本地假服务完成一次请求，再关闭假服务（`server.shutdown()` 加 `server_close()`），然后断言下一次请求报"连不上"——结果请求成功返回了，断言 DID NOT RAISE。

**原因**：`server_close()` 只关监听套接字，不再接受新连接；已经建立的连接由各自的处理线程持有，仍然可读可写。客户端 Session 的连接池里正好留着这条 keep-alive 连接，下一次请求直接复用了它。

**解决**：要模拟"连不上"，让客户端指向一个确定没人监听的地址——先 bind 到 0 号端口拿到一个空闲端口号再关掉，用它拼 URL。附带一个现象：Windows 上连接本机一个拒绝连接的端口，要等约 2 秒才报告"连接被拒"，所以这类用例单条就要 2 秒，属正常情况。

### 54. 测试替身漏了真实现的一个属性：被测线程静默退出，整套用例全红（2026-11-26）

**现象**：主循环的 22 个用例里 21 个红，每个都要等到超时，整套跑了 99 秒。写个脚本把循环线程里的异常原样打出来才看到：`AttributeError: 'FakeClient' object has no attribute 'agent_id'`——循环在注册成功后读这个属性，线程当场退出，之后所有"等到某条件成立"的断言都只能等到超时。

**原因**：假客户端是照着脑子里的接口写的，漏了真客户端的 `agent_id`；而循环跑在子线程里，异常只被 pytest 记成一条 warning，不会让用例直接失败。与 #31、#47 同源：替身与真实现的差异，让问题换了一种更难认的形态出现。

**解决**：①写替身之前对着真实现的源码把接口列一遍，用到的方法和属性逐个对上。②被测代码跑在子线程时，收尾断言里带上"线程还活着且没有未处理的异常"，让根因第一时间暴露，而不是表现为一堆超时。教训：**大批量同时变红而且都是超时，先怀疑测试设施，再怀疑被测代码**——先把线程里的异常原样打出来。

## 脚本与工具（续 2）

### 55. 设了 `MSYS_NO_PATHCONV=1` 之后，原生程序收到的 `/tmp/...` 打不开（2026-11-26）

**现象**：Git Bash 里 `T=$(mktemp -d)` 得到 `/tmp/xxx`，作为参数传给原生的 `python` 脚本去读写，报 `FileNotFoundError: '\\tmp\\xxx\\...'`。同一条命令里，前面一次同样的调用是好的。

**原因**：Git Bash 默认把长得像 POSIX 路径的参数转成 Windows 路径再传给原生程序。为了不让 docker 的 `/app/...` 被改写（#11）而 `export MSYS_NO_PATHCONV=1` 之后，这个转换对**所有**参数都关掉了：`/tmp/xxx` 原样传给 Python，被解析成当前盘符根下的 `\tmp\xxx`。`export` 之后的每条命令都受影响，所以只设了一次的变量会让后面不相关的步骤出错。

**解决**：给原生程序的路径先用 `cygpath -m "$T"` 转成 `C:/...` 形式再传；`MSYS_NO_PATHCONV=1` 尽量只加在那一条 docker 命令前面，不要 `export`。同一天还踩到三个同类的小坑，一并记：`docker run` 不带 `-i` 时，`python -` 从 stdin 读到的是空的，脚本看起来"跑了"其实一行没执行（#33 的同型）；`subprocess.run` 不能同时给 `capture_output=True` 和 `stderr=`，要显式写 `stdout=subprocess.PIPE`；验证某个命令"会不会自动建目录"时别用 `/xxx` 当测试路径——Git Bash 会把它转到自己的安装目录下（当天在 Git 安装目录里留下一个测试文件夹，已删）。

## 容器与 Docker（续）

### 56. 用 `timeout` 包住 `docker run --rm` 做自检：客户端被杀了，容器还在跑（2026-11-26）

**现象**：给新构建的 Agent 镜像做启动自检，命令形如 `timeout 14 docker run --rm <镜像>`。到点后命令返回，看上去容器已经结束。一个多小时后 fresh 评审发现有一个随机名字的容器（`exciting_nash`）从自检那一刻起一直在跑，每 30 秒打一行注册失败的日志。

**原因**：`timeout` 杀掉的是本机的 `docker run` 客户端进程，容器本身不受影响，继续在后台运行。`--rm` 只在容器**退出之后**才删容器，而镜像里跑的是不会自己退出的常驻循环，所以永远等不到清理。没指定 `--name`，留下的是一个随机名容器，`docker ps` 里混在别的容器中间不容易被注意到。

**解决**：这类"起来看一眼日志"的自检不要用 `timeout` 包 `docker run`。起一个带固定名字的：`docker run -d --rm --name brp-agent-smoke <镜像>`，看完 `docker logs` 后 `docker stop brp-agent-smoke`——`--rm` 会在停止后自动删除。收工前用 `docker ps -a` 核对没有计划外的容器。教训：**`--rm` 管的是"退出后删除"，不保证进程会退出**；让容器停下来的动作要显式做，不能靠杀客户端。
