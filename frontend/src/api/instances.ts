import { client } from './client'

// 后端契约：GET /instances 列表（health 为对象或 null；响应永不含密码字段）
export interface CheckResult {
  checked_at: string
  reachable: boolean
  latency_ms: number
  error?: string
}

export interface InstanceRow {
  id: number
  name: string
  kind: 'external' | 'experimental'
  host: string
  amqp_port: number
  mgmt_port: number
  vhost: string
  username: string
  health: CheckResult | null
  created_at: string
  updated_at: string
}

// 登记弹窗显式传全部默认值（与后端表 15 缺省一致，见当日裁决）
export interface RegisterPayload {
  name: string
  kind: string
  host: string
  amqp_port: number
  mgmt_port: number
  vhost: string
  username: string
  password: string
}

export async function listInstances(): Promise<InstanceRow[]> {
  const { data } = await client.get<InstanceRow[]>('/instances')
  return data
}

export async function registerInstance(payload: RegisterPayload): Promise<void> {
  await client.post('/instances', payload)
}

export async function checkInstance(id: number): Promise<void> {
  await client.post(`/instances/${id}/check`)
}

export async function getInstanceHealth(id: number): Promise<CheckResult | null> {
  const { data } = await client.get<{ health: CheckResult | null }>(`/instances/${id}/health`)
  return data.health
}
