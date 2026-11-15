# 实验辅助三合一镜像（M1 第 20 天）：producer / consumer / recorder 共用，
# 被 deploy/compose/lab-template.yml 引用（image: brp-lab:dev）。
# helpers 与仓库同源（COPY 整个 lab 目录，禁散装复制）；/app + PYTHONPATH 使
# lab.helpers.* 包路径在容器内成立（第 19 天验收踩过的挂载经验固化进镜像）。
FROM python:3.13-slim

COPY requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir -i https://pypi.tuna.tsinghua.edu.cn/simple -r /tmp/requirements.txt

COPY . /app/lab
ENV PYTHONPATH=/app

# 角色入口由 lab-template 的 service command 决定（recorder 已换真；
# producer/consumer 第 21 天 conftest 参数化注入，占位保持存活）
CMD ["sleep", "infinity"]
