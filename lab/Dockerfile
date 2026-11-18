# 实验辅助三合一镜像（M1 第 20 天）：producer / consumer / recorder 共用，
# 被 deploy/compose/lab-template.yml 引用（image: brp-lab:dev）。
# helpers 与仓库同源（COPY 整个 lab 目录，禁散装复制）；/app + PYTHONPATH 使
# lab.helpers.* 包路径在容器内成立（第 19 天验收踩过的挂载经验固化进镜像）。
# **双向打包依赖（day25 评审 M-3）**：场景套件（test_flow）import agent.reconcile.*——
# 手跑 runner 须另挂 agent 卷；M2 打包若场景随 agent 容器跑则无此问题，若独立打包须双向 COPY。
FROM python:3.13-slim

COPY requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir -i https://pypi.tuna.tsinghua.edu.cn/simple -r /tmp/requirements.txt

COPY . /app/lab
ENV PYTHONPATH=/app

# 角色入口由 lab-template 的 service command 决定（recorder 已换真；
# producer/consumer 第 21 天 conftest 参数化注入，占位保持存活）
CMD ["sleep", "infinity"]
