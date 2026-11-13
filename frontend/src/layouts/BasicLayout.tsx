import { Outlet, useNavigate } from 'react-router-dom'
import { Button, Layout } from 'antd'

import { useAuthStore } from '../stores/authStore'

// 主布局骨架：Header（标题 + 退出）+ Content（子路由 Outlet）。
// 侧边菜单（实例/场景/任务/报告）第 12 天随实例页接入。
export default function BasicLayout() {
  const navigate = useNavigate()
  const logout = useAuthStore((s) => s.logout)

  return (
    <Layout style={{ minHeight: '100vh' }}>
      <Layout.Header
        style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', color: '#fff' }}
      >
        <span style={{ fontSize: 16, fontWeight: 600 }}>BRP 控制台</span>
        <Button
          type="text"
          style={{ color: '#fff' }}
          onClick={() => {
            logout()
            navigate('/login', { replace: true })
          }}
        >
          退出登录
        </Button>
      </Layout.Header>
      <Layout.Content style={{ padding: 24 }}>
        <Outlet />
      </Layout.Content>
    </Layout>
  )
}
