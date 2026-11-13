# ---- 构建阶段：Vite 构建静态产物（镜像内完成，不依赖宿主机 dist）----
FROM node:24-alpine AS build
# 国内镜像源（与宿主机 npm 口径一致）
RUN npm config set registry https://registry.npmmirror.com
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
RUN npm run build

# ---- 运行阶段：nginx 托管静态产物 ----
FROM nginx:1.27-alpine
COPY --from=build /app/dist /usr/share/nginx/html
# server 配置（SPA 回退 + /api 反代 + SSE 预备）由 compose 挂载
# deploy/nginx/nginx.conf → /etc/nginx/conf.d/default.conf，dev 可热改
EXPOSE 80
