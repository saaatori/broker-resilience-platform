# Agent 镜像（占位阶段零三方依赖；M2 引入 uv + 锁文件后改为 uv sync 安装）。
# M2 打包约束（day23 评审 M-10）：reconcile/counter 依赖 lab.helpers.events——
# 届时必须 COPY lab/helpers/events.py（或提炼共享包），且 envmgr/ 与 deploy/ 模板同装。
FROM python:3.13-slim

ENV PYTHONUNBUFFERED=1 \
    TZ=Asia/Shanghai

WORKDIR /app
COPY pyproject.toml main.py ./

CMD ["python", "main.py"]
