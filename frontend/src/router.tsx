import { useEffect, useState } from 'react'
import { isAxiosError } from 'axios'
import { Button } from 'antd'
import { Navigate, Outlet, Route, Routes } from 'react-router-dom'

import { me } from './api/auth'
import BasicLayout from './layouts/BasicLayout'
import LoginPage from './pages/login'
import { useAuthStore } from './stores/authStore'

// AuthGuard：路由守卫。渲染期判 token 存在性（不存在/被清除立即回登录页），
// effect 只负责调 /me 做服务端验证——过期/伪造 token 只有后端能判定。
// 401 → logout 清 token（与拦截器同款），渲染守卫随即导航（单通道，无 location 硬跳）；
// 网络故障（非 401）不清 token——后端瞬断不应把人踢出登录态（评审 #3）。
function AuthGuard() {
  const token = useAuthStore((s) => s.token)
  const logout = useAuthStore((s) => s.logout)
  const [state, setState] = useState<'checking' | 'ok' | 'error'>('checking')
  const [retry, setRetry] = useState(0)

  useEffect(() => {
    let cancelled = false
    me()
      .then(() => {
        if (!cancelled) setState('ok')
      })
      .catch((e: unknown) => {
        if (cancelled) return
        if (isAxiosError(e) && e.response?.status === 401) {
          logout()
        } else {
          setState('error')
        }
      })
    return () => {
      cancelled = true
    }
  }, [token, logout, retry])

  if (!token) return <Navigate to="/login" replace />
  if (state === 'checking') return <div>鉴权中…</div>
  if (state === 'error') {
    return (
      <div style={{ padding: 24 }}>
        无法连接服务器，请确认后端已启动。
        <Button type="link" onClick={() => setRetry((r) => r + 1)}>
          重试
        </Button>
      </div>
    )
  }
  return <Outlet />
}

// 首页占位：实例/场景/任务/报告页面自第 12 天起接入。
function Home() {
  return <div>欢迎回来——首页占位（实例/场景/任务页面自第 12 天起接入）</div>
}

export default function AppRoutes() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route element={<AuthGuard />}>
        <Route path="/" element={<BasicLayout />}>
          <Route index element={<Home />} />
        </Route>
      </Route>
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  )
}
