import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { Button, Card, Form, Input, message } from 'antd'

import { login } from '../../api/auth'
import { apiError } from '../../api/client'
import { useAuthStore } from '../../stores/authStore'

// 登录页：成功存 token 进主布局；失败透出后端错误文案（401 → "用户名或密码错误"）。
export default function LoginPage() {
  const navigate = useNavigate()
  const setToken = useAuthStore((s) => s.setToken)
  const [loading, setLoading] = useState(false)

  const onFinish = async (values: { username: string; password: string }) => {
    setLoading(true)
    try {
      const token = await login(values.username, values.password)
      setToken(token)
      message.success('登录成功')
      navigate('/', { replace: true })
    } catch (e) {
      message.error(apiError(e))
    } finally {
      setLoading(false)
    }
  }

  return (
    <div
      style={{
        display: 'flex',
        justifyContent: 'center',
        alignItems: 'center',
        minHeight: '100vh',
        background: '#f5f5f5',
      }}
    >
      <Card title="BRP 控制台登录" style={{ width: 360 }}>
        <Form layout="vertical" onFinish={onFinish} autoComplete="off">
          <Form.Item name="username" label="用户名" rules={[{ required: true, message: '请输入用户名' }]}>
            <Input placeholder="admin" />
          </Form.Item>
          <Form.Item name="password" label="密码" rules={[{ required: true, message: '请输入密码' }]}>
            <Input.Password />
          </Form.Item>
          <Button type="primary" htmlType="submit" block loading={loading}>
            登录
          </Button>
        </Form>
      </Card>
    </div>
  )
}
