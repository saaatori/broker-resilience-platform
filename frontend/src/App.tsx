import { BrowserRouter } from 'react-router-dom'
import { ConfigProvider } from 'antd'
import zhCN from 'antd/locale/zh_CN'

import AppRoutes from './router'

// 应用壳：antd 中文语境 + 路由器；具体路由与守卫见 router.tsx。
export default function App() {
  return (
    <ConfigProvider locale={zhCN}>
      <BrowserRouter>
        <AppRoutes />
      </BrowserRouter>
    </ConfigProvider>
  )
}
