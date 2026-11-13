# Agent 镜像（占位阶段零三方依赖；M2 引入 uv + 锁文件后改为 uv sync 安装）
FROM python:3.13-slim

ENV PYTHONUNBUFFERED=1 \
    TZ=Asia/Shanghai

WORKDIR /app
COPY pyproject.toml main.py ./

CMD ["python", "main.py"]
