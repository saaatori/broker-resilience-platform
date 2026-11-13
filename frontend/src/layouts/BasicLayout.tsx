import { Outlet, useLocation, useNavigate } from 'react-router-dom'
import { Button, Layout, Menu } from 'antd'

import { useAuthStore } from '../stores/authStore'

// 主布局：Header（标题 + 退出）+ Sider 菜单 + Content（子路由 Outlet）。
// 场景/任务/报告为后续里程碑占位（禁用态），对应页面接入后启用。
const menuItems = [
  { key: '/', label: '首页' },
  { key: '/instances', label: '实例管理' },
  { key: '/scenarios', label: '场景库', disabled: true },
  { key: '/tasks', label: '任务中心', disabled: true },
  { key: '/reports', label: '报告', disabled: true },
]

export default function BasicLayout() {
  const navigate = useNavigate()
  const location = useLocation()
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
      <Layout>
        <Layout.Sider width={180} theme="dark">
          <Menu
            theme="dark"
            mode="inline"
            selectedKeys={[location.pathname]}
            items={menuItems}
            onClick={(e) => navigate(e.key)}
          />
        </Layout.Sider>
        <Layout.Content style={{ padding: 24 }}>
          <Outlet />
        </Layout.Content>
      </Layout>
    </Layout>
  )
}
