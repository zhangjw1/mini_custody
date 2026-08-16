import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Alert, App, Button, Form, Input, Result, Spin, Steps } from 'antd'
import { ArrowUpFromLine, Calculator, ExternalLink } from 'lucide-react'
import { api } from '../api'
import { useAppContext } from '../AppContext'
import { addETH, formatDate, formatETH, HashLink, PageHeader, QueryState, SectionHeader, StatusTag } from '../components/Common'
import type { Withdrawal, WithdrawalQuote } from '../types'

type WithdrawForm = { to_address: string; amount_eth: string }

export default function WithdrawPage() {
  const { modal } = App.useApp()
  const { userId } = useAppContext()
  const queryClient = useQueryClient()
  const [form] = Form.useForm<WithdrawForm>()
  const [quote, setQuote] = useState<WithdrawalQuote>()
  const [pendingValues, setPendingValues] = useState<WithdrawForm>()
  const [created, setCreated] = useState<Withdrawal>()
  const walletQuery = useQuery({ queryKey: ['wallet', userId], queryFn: () => api.wallet(userId), enabled: userId > 0 })
  const detailQuery = useQuery({
    queryKey: ['withdrawal', created?.id],
    queryFn: () => api.withdrawal(created!.id),
    enabled: Boolean(created?.id),
    refetchInterval: (query) => ['COMPLETED', 'FAILED'].includes(query.state.data?.status ?? '') ? false : 3_000,
  })
  const currentWithdrawal = detailQuery.data ?? created

  useEffect(() => {
    setQuote(undefined)
    setPendingValues(undefined)
    setCreated(undefined)
    form.resetFields()
  }, [userId, form])

  useEffect(() => {
    if (detailQuery.data && ['COMPLETED', 'FAILED'].includes(detailQuery.data.status)) {
      queryClient.invalidateQueries({ queryKey: ['wallet', userId] })
      queryClient.invalidateQueries({ queryKey: ['transactions'] })
      queryClient.invalidateQueries({ queryKey: ['withdrawals', userId] })
    }
  }, [detailQuery.data, queryClient, userId])

  const quoteMutation = useMutation({
    mutationFn: (values: WithdrawForm) => api.quoteWithdrawal(userId, values),
    onSuccess: (data, values) => {
      setQuote(data)
      setPendingValues(values)
    },
  })
  const createMutation = useMutation({
    mutationFn: (values: WithdrawForm) => api.createWithdrawal(userId, crypto.randomUUID(), values),
    onSuccess: (data) => {
      setCreated(data)
      setPendingValues(undefined)
      queryClient.invalidateQueries({ queryKey: ['wallet', userId] })
    },
  })

  const requestQuote = async () => {
    const values = await form.validateFields()
    setQuote(undefined)
    quoteMutation.mutate(values)
  }

  const confirmWithdrawal = () => {
    if (!pendingValues || !quote) return
    modal.confirm({
      title: '确认提交提币',
      content: (
        <div className="confirm-details">
          <p><span>到账地址</span><strong className="mono break-all">{pendingValues.to_address}</strong></p>
          <p><span>提币金额</span><strong>{pendingValues.amount_eth} ETH</strong></p>
          <p><span>最大网络费</span><strong>{quote.reserved_fee_eth} ETH</strong></p>
          <p><span>最大扣减</span><strong>{addETH(pendingValues.amount_eth, quote.reserved_fee_eth)} ETH</strong></p>
        </div>
      ),
      okText: '确认提币',
      cancelText: '取消',
      okButtonProps: { danger: true },
      onOk: () => createMutation.mutateAsync(pendingValues),
    })
  }

  if (currentWithdrawal) {
    const step = currentWithdrawal.status === 'COMPLETED' ? 3 : currentWithdrawal.status === 'FAILED' ? 1 : currentWithdrawal.tx_hash ? 2 : 1
    return (
      <>
        <PageHeader title="提币进度" />
        <section className="panel withdrawal-result">
          <Result
            status={currentWithdrawal.status === 'FAILED' ? 'error' : currentWithdrawal.status === 'COMPLETED' ? 'success' : 'info'}
            title={currentWithdrawal.status === 'COMPLETED' ? '提币已完成' : currentWithdrawal.status === 'FAILED' ? '提币失败' : '提币处理中'}
            subTitle={`提币单 #${currentWithdrawal.id}`}
          />
          <Steps current={step} status={currentWithdrawal.status === 'FAILED' ? 'error' : 'process'} items={[{ title: '已创建' }, { title: '签名广播' }, { title: '链上确认' }, { title: '完成' }]} />
          <div className="withdrawal-detail-grid">
            <div><span>状态</span><StatusTag status={currentWithdrawal.status} /></div>
            <div><span>金额</span><strong>{currentWithdrawal.amount_eth} ETH</strong></div>
            <div><span>预留网络费</span><strong>{currentWithdrawal.reserved_fee_eth} ETH</strong></div>
            <div><span>实际网络费</span><strong>{currentWithdrawal.actual_fee_eth ? `${currentWithdrawal.actual_fee_eth} ETH` : '--'}</strong></div>
            <div><span>确认数</span><strong>{currentWithdrawal.confirmations}</strong></div>
            <div><span>创建时间</span><strong>{formatDate(currentWithdrawal.created_at)}</strong></div>
            <div className="wide"><span>交易哈希</span><HashLink hash={currentWithdrawal.tx_hash} url={currentWithdrawal.explorer_url} /></div>
          </div>
          {currentWithdrawal.error_message && <Alert type="error" showIcon title={currentWithdrawal.error_message} />}
          <div className="result-actions">
            <Button onClick={() => { setCreated(undefined); setQuote(undefined); form.resetFields() }}>发起新的提币</Button>
            {currentWithdrawal.explorer_url && <Button icon={<ExternalLink size={16} />} href={currentWithdrawal.explorer_url} target="_blank">区块浏览器</Button>}
          </div>
        </section>
      </>
    )
  }

  return (
    <>
      <PageHeader title="提币 ETH" />
      <div className="withdraw-layout">
        <section className="panel withdraw-form-panel">
          <SectionHeader title="填写提币信息" />
          <QueryState loading={walletQuery.isLoading} error={walletQuery.error} retry={() => walletQuery.refetch()} />
          {walletQuery.data && <div className="available-line"><span>可用余额</span><strong>{formatETH(walletQuery.data.balance.available_eth)} ETH</strong></div>}
          <Form form={form} layout="vertical" requiredMark={false} onValuesChange={() => { setQuote(undefined); setPendingValues(undefined) }}>
            <Form.Item label="到账地址" name="to_address" rules={[{ required: true, message: '请输入到账地址' }, { pattern: /^0x[a-fA-F0-9]{40}$/, message: '请输入有效的 EVM 地址' }]}>
              <Input size="large" className="mono" placeholder="0x..." autoComplete="off" />
            </Form.Item>
            <Form.Item label="提币金额" name="amount_eth" rules={[{ required: true, message: '请输入提币金额' }, { pattern: /^(?:0|[1-9]\d*)(?:\.\d{1,18})?$/, message: '请输入最多 18 位小数的有效金额' }]}>
              <Input size="large" suffix="ETH" inputMode="decimal" placeholder="0.001" autoComplete="off" />
            </Form.Item>
            {quoteMutation.isError && <Alert className="form-alert" type="error" showIcon title={quoteMutation.error.message} />}
            <Button block size="large" type="primary" icon={quoteMutation.isPending ? <Spin size="small" /> : <Calculator size={17} />} onClick={requestQuote} disabled={quoteMutation.isPending}>试算网络费</Button>
          </Form>
        </section>

        <aside className="panel quote-panel">
          <SectionHeader title="费用预览" />
          {quote ? (
            <>
              <div className="quote-rows">
                <p><span>提币金额</span><strong>{quote.amount_eth} ETH</strong></p>
                <p><span>Gas Limit</span><strong>{quote.gas_limit.toLocaleString()}</strong></p>
                <p><span>最大网络费</span><strong>{quote.reserved_fee_eth} ETH</strong></p>
              </div>
              <Alert type="info" showIcon title="实际网络费按链上执行结果结算，未使用的预留费用会返还余额。" />
              {createMutation.isError && <Alert className="form-alert" type="error" showIcon title={createMutation.error.message} />}
              <Button block size="large" danger type="primary" icon={<ArrowUpFromLine size={17} />} onClick={confirmWithdrawal} loading={createMutation.isPending}>提交提币</Button>
            </>
          ) : (
            <div className="quote-empty"><Calculator size={28} /><span>填写地址和金额后试算网络费</span></div>
          )}
        </aside>
      </div>
    </>
  )
}
