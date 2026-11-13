import { useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Button,
  Card,
  Descriptions,
  Form,
  Input,
  InputNumber,
  Modal,
  Select,
  Space,
  Table,
  Tag,
  message,
} from 'antd'
import type { ColumnsType } from 'antd/es/table'

import { apiError } from '../../api/client'
import {
  type CheckResult,
  type InstanceRow,
  type RegisterPayload,
  checkInstance,
  getInstanceHealth,
  listInstances,
  registerInstance,
} from '../../api/instances'

const kindText: Record<string, string> = { external: '外部', experimental: '实验' }

// 实例管理页：列表（TanStack Query）+ 登记弹窗 + 行内检测/健康（对接第 8/9/12 天端点）。
export default function InstancesPage() {
  const queryClient = useQueryClient()
  const { data: rows, isPending } = useQuery({ queryKey: ['instances'], queryFn: listInstances })

  const [registerOpen, setRegisterOpen] = useState(false)
  const [form] = Form.useForm<RegisterPayload>()
  const [healthRow, setHealthRow] = useState<InstanceRow | null>(null)
  const [healthDetail, setHealthDetail] = useState<CheckResult | null>(null)
  const [healthLoading, setHealthLoading] = useState(false)

  const registerMut = useMutation({
    mutationFn: registerInstance,
    onSuccess: async () => {
      message.success('登记成功')
      setRegisterOpen(false)
      form.resetFields()
      await queryClient.invalidateQueries({ queryKey: ['instances'] })
    },
    onError: (e) => message.error(apiError(e)),
  })

  const checkMut = useMutation({
    mutationFn: checkInstance,
    onSuccess: async () => {
      message.success('检测完成')
      await queryClient.invalidateQueries({ queryKey: ['instances'] })
    },
    onError: (e) => message.error(apiError(e)),
  })

  // 健康详情：点击时现查（保证最新），失败透后端文案。
  // seq 防竞态：快速连点两行时，晚返回的过期响应整组丢弃（评审 Important）。
  const healthSeq = useRef(0)
  const openHealth = async (row: InstanceRow) => {
    const seq = ++healthSeq.current
    setHealthRow(row)
    setHealthDetail(null)
    setHealthLoading(true)
    try {
      const h = await getInstanceHealth(row.id)
      if (seq !== healthSeq.current) return
      setHealthDetail(h)
    } catch (e) {
      if (seq !== healthSeq.current) return
      message.error(apiError(e))
      setHealthRow(null)
    } finally {
      if (seq === healthSeq.current) setHealthLoading(false)
    }
  }

  const columns: ColumnsType<InstanceRow> = [
    { title: 'ID', dataIndex: 'id', width: 60 },
    { title: '名称', dataIndex: 'name' },
    {
      title: '类型',
      dataIndex: 'kind',
      width: 90,
      render: (k: string) => <Tag>{kindText[k] ?? k}</Tag>,
    },
    {
      title: '地址',
      key: 'addr',
      render: (_, r) => `${r.host}:${r.amqp_port}`,
    },
    { title: '用户名', dataIndex: 'username', width: 110 },
    {
      title: '最近检测',
      key: 'health',
      width: 170,
      render: (_, r) => {
        if (!r.health) return <Tag>未检测</Tag>
        return (
          <Space size={4}>
            <Tag color={r.health.reachable ? 'green' : 'red'}>
              {r.health.reachable ? '正常' : '异常'}
            </Tag>
            <span style={{ color: '#888', fontSize: 12 }}>{r.health.latency_ms}ms</span>
          </Space>
        )
      },
    },
    {
      title: '操作',
      key: 'ops',
      width: 170,
      render: (_, r) => (
        <Space>
          <Button
            size="small"
            loading={checkMut.isPending && checkMut.variables === r.id}
            onClick={() => checkMut.mutate(r.id)}
          >
            检测
          </Button>
          <Button size="small" onClick={() => void openHealth(r)}>
            健康
          </Button>
        </Space>
      ),
    },
  ]

  return (
    <Card
      title="中间件实例"
      extra={
        <Button type="primary" onClick={() => setRegisterOpen(true)}>
          登记实例
        </Button>
      }
    >
      <Table<InstanceRow>
        rowKey="id"
        size="middle"
        loading={isPending}
        columns={columns}
        dataSource={rows ?? []}
        pagination={false}
      />

      <Modal
        title="登记实例"
        open={registerOpen}
        onCancel={() => setRegisterOpen(false)}
        onOk={() => form.submit()}
        confirmLoading={registerMut.isPending}
        destroyOnHidden
      >
        <Form
          form={form}
          layout="vertical"
          initialValues={{ kind: 'external', amqp_port: 5672, mgmt_port: 15672, vhost: '/' }}
          onFinish={(v) => registerMut.mutate(v)}
        >
          <Form.Item name="name" label="名称" rules={[{ required: true, message: '请输入名称' }, { max: 128 }]}>
            <Input placeholder="local-rabbit" />
          </Form.Item>
          <Form.Item name="kind" label="类型">
            <Select
              options={[
                { value: 'external', label: '外部（external）' },
                { value: 'experimental', label: '实验（experimental）' },
              ]}
            />
          </Form.Item>
          <Form.Item name="host" label="主机" rules={[{ required: true, message: '请输入主机地址' }, { max: 255 }]}>
            <Input placeholder="127.0.0.1" />
          </Form.Item>
          <Space size="middle">
            <Form.Item
              name="amqp_port"
              label="AMQP 端口"
              rules={[{ type: 'number', min: 1, max: 65535, message: '1-65535' }]}
            >
              <InputNumber style={{ width: 130 }} />
            </Form.Item>
            <Form.Item
              name="mgmt_port"
              label="管理端口"
              rules={[{ type: 'number', min: 1, max: 65535, message: '1-65535' }]}
            >
              <InputNumber style={{ width: 130 }} />
            </Form.Item>
          </Space>
          <Form.Item name="vhost" label="vhost" rules={[{ max: 128 }]}>
            <Input style={{ width: 130 }} />
          </Form.Item>
          <Form.Item name="username" label="用户名" rules={[{ required: true, message: '请输入用户名' }, { max: 64 }]}>
            <Input />
          </Form.Item>
          <Form.Item name="password" label="密码" rules={[{ required: true, message: '请输入密码' }]}>
            <Input.Password />
          </Form.Item>
        </Form>
      </Modal>

      <Modal
        title={healthRow ? `健康详情：${healthRow.name}` : '健康详情'}
        open={!!healthRow}
        footer={null}
        onCancel={() => setHealthRow(null)}
      >
        {healthLoading ? (
          <div>加载中…</div>
        ) : healthDetail ? (
          <Descriptions column={1} size="small" bordered>
            <Descriptions.Item label="可达性">
              <Tag color={healthDetail.reachable ? 'green' : 'red'}>
                {healthDetail.reachable ? '正常' : '异常'}
              </Tag>
            </Descriptions.Item>
            <Descriptions.Item label="检测时间">{healthDetail.checked_at}</Descriptions.Item>
            <Descriptions.Item label="耗时">{healthDetail.latency_ms} ms</Descriptions.Item>
            {healthDetail.error ? (
              <Descriptions.Item label="失败原因">{healthDetail.error}</Descriptions.Item>
            ) : null}
          </Descriptions>
        ) : (
          <div>尚未检测</div>
        )}
      </Modal>
    </Card>
  )
}
