# Agent 镜像（M2 第 38 天起按锁文件装依赖；此前是零依赖占位）。
# 版本与文件哈希以 uv.lock 为准，但不用 `uv sync --frozen` 下载：它按锁内记录的地址取 wheel，而锁绑定的
# 清华镜像在本机返回 403（排障 #49）；--frozen 下改不了地址，换源重锁又会整体改写 uv.lock（排障 #42）。
# 做法：导出阶段用 uv 离线导出"精确版本 + sha256"清单 → 运行阶段 pip --require-hashes 安装，
# 拿到的仍是锁定的同一批文件；uv 本体不进运行阶段。
# 安装源可经构建参数覆盖（国内网络慢时）：
#   docker compose -f deploy/compose/platform.yml --env-file deploy/env/.env \
#     build --build-arg PIP_INDEX_URL=https://mirrors.cloud.tencent.com/pypi/simple agent
# M2 打包约束（day23 评审 M-10）：reconcile/counter 依赖 lab.helpers.events——
# 届时必须 COPY lab/helpers/events.py（或提炼共享包），且 envmgr/ 与 deploy/ 模板同装。

# ---- 导出阶段：uv.lock → 带哈希的依赖清单 ----
FROM ghcr.io/astral-sh/uv:0.12.17 AS export
WORKDIR /src
COPY pyproject.toml uv.lock ./
# 该镜像不含 shell（distroless）：RUN 只能用 exec 形式；uv 在 /uv 而不在 $PATH；输出目录须已存在（WORKDIR 建的 /src）
RUN ["/uv", "export", "--frozen", "--offline", "--no-dev", "--no-emit-project", "--no-header", "-o", "requirements.txt"]

# ---- 运行阶段 ----
FROM python:3.13-slim

ENV PYTHONUNBUFFERED=1 \
    TZ=Asia/Shanghai

ARG PIP_INDEX_URL=https://pypi.org/simple
WORKDIR /app
# 先装依赖再拷代码：改代码不失效依赖层
COPY --from=export /src/requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir --require-hashes -i "$PIP_INDEX_URL" -r /tmp/requirements.txt

COPY main.py ./
COPY executor/ ./executor/

CMD ["python", "main.py"]
