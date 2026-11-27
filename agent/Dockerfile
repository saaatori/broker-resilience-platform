# Agent 镜像（M2 第 40 天）：布局镜像仓库根 /brp——agent/ lab/ deploy/ 同装（day23 M-10 兑现）。
# 构建上下文=仓库根（lab/ 与 deploy/ 模板在上下文内），dockerfile 指到 agent/Dockerfile。
# PYTHONPATH 双路径与 agent/tests/conftest 同构：/brp/agent（executor.*、runner.* 绝对导入）
# + /brp（lab.*、agent.*——场景 test_flow import agent.reconcile）。
# 依赖：uv.lock 哈希清单为主（版本与文件哈希以锁为准）；pytest/pika 直装不带哈希——
# pytest 原在 dev 组被 --no-dev 排除、pika 不在锁内，而补锁须换源重锁（清华镜像 403，
# AGENTS.md §9 禁止提交换源锁）——版本区间钉住，哈希校验留待镜像站恢复后收编。
FROM ghcr.io/astral-sh/uv:0.12.17 AS export
WORKDIR /src
COPY agent/pyproject.toml agent/uv.lock ./
# 该镜像不含 shell（distroless）：RUN 须 exec 形式；uv 在 /uv 不在 $PATH；输出目录须已存在（WORKDIR 建的 /src）
RUN ["/uv", "export", "--frozen", "--offline", "--no-dev", "--no-emit-project", "--no-header", "-o", "requirements.txt"]

# ---- 运行阶段 ----
FROM python:3.13-slim

ENV PYTHONUNBUFFERED=1 \
    TZ=Asia/Shanghai \
    PYTHONPATH=/brp/agent

ARG PIP_INDEX_URL=https://pypi.org/simple
WORKDIR /brp/agent
# 先装依赖再拷代码：改代码不失效依赖层
COPY --from=export /src/requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir --require-hashes -i "$PIP_INDEX_URL" -r /tmp/requirements.txt \
    && pip install --no-cache-dir -i "$PIP_INDEX_URL" "pytest>=8,<10" "pika>=1.3,<2"

COPY agent/ /brp/agent/
COPY lab/ /brp/lab/
# deploy 只装工厂实际读的两个文件（lab_factory.py 的 TEMPLATE_COMPOSE / TEMPLATE_TOXIPROXY）——
# 整目录 COPY 会把 deploy/env/.env（全套平台口令）带进镜像（评审 I-1）
COPY deploy/compose/lab-template.yml /brp/deploy/compose/lab-template.yml
COPY deploy/lab/toxiproxy.json /brp/deploy/lab/toxiproxy.json

CMD ["python", "main.py"]
