import { client } from './client'

// 后端契约（第 5 天）：POST /api/v1/auth/login → 200 {"token"}；错密 401 {"error"}。
export async function login(username: string, password: string): Promise<string> {
  const { data } = await client.post<{ token: string }>('/auth/login', { username, password })
  return data.token
}

// 后端契约（第 6 天）：GET /api/v1/me → 200 {"username"}；无效/过期 token 401。
// 供 AuthGuard 做服务端验证——纯客户端 token 存在性检查挡不住过期/伪造 token。
export async function me(): Promise<string> {
  const { data } = await client.get<{ username: string }>('/me')
  return data.username
}
