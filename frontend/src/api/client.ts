import axios from 'axios'

import { useAuthStore } from '../stores/authStore'

// 全站唯一 Axios 实例：baseURL /api/v1（dev 由 Vite 代理转发到本机 8080，浏览器同源无 CORS）。
export const client = axios.create({ baseURL: '/api/v1', timeout: 10_000 })

client.interceptors.request.use((config) => {
  const token = useAuthStore.getState().token
  if (token) config.headers.Authorization = `Bearer ${token}`
  return config
})

client.interceptors.response.use(undefined, (error) => {
  // 401 统一清登录态；页面跳转由 AuthGuard 响应 token 变化完成——
  // 单通道设计：不做 window.location 硬跳，避免与 SPA 导航赛跑（评审 #4）。
  // 登录接口自身错密也是 401，logout 幂等无害。
  if (axios.isAxiosError(error) && error.response?.status === 401) {
    useAuthStore.getState().logout()
  }
  return Promise.reject(error)
})

// apiError 把任意异常转成可展示的中文文案：优先透出后端 {"error": "..."}，不裸抛 AxiosError。
export function apiError(e: unknown): string {
  if (axios.isAxiosError(e)) {
    const msg = (e.response?.data as { error?: string } | undefined)?.error
    if (msg) return msg
    if (!e.response) return '网络错误，请稍后重试'
  }
  return '请求失败，请稍后重试'
}
