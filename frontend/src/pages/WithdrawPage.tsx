import { useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Alert, App, Button, Form, Input, Result, Spin, Steps } from 'antd'
import { ArrowUpFromLine, Calculator, ExternalLink } from 'lucide-react'
import { api } from '../api'
import { useAppContext } from '../AppContext'
import { addETH, formatDate, formatETH, HashLink, PageHeader, QueryState, SectionHeader, StatusTag } from '../components/Common'
import type { TokenWithdrawal, TokenWithdrawalQuote, Withdrawal, WithdrawalQuote } from '../types'
import BitcoinWithdrawPanel from './BitcoinWithdrawPanel'

type WithdrawForm = { to_address: string; amount: string }
type Quote = { kind: 'ETH'; data: WithdrawalQuote } | { kind: 'TOKEN'; data: TokenWithdrawalQuote }
type Created = { kind: 'ETH'; data: Withdrawal } | { kind: 'TOKEN'; data: TokenWithdrawal }

export default function WithdrawPage() {
  const { modal } = App.useApp()
  const { userId, asset } = useAppContext()
  const queryClient = useQueryClient()
  const [form] = Form.useForm<WithdrawForm>()
  const [quote, setQuote] = useState<Quote>()
  const [pendingValues, setPendingValues] = useState<WithdrawForm>()
  const [created, setCreated] = useState<Created>()
  const isToken = asset.asset_type === 'ERC20'
  const balancesQuery = useQuery({ queryKey: ['balances', userId], queryFn: () => api.balances(userId), enabled: userId > 0 })
  const selectedBalance = useMemo(() => balancesQuery.data?.items.find((item) => item.asset === asset.symbol), [asset.symbol, balancesQuery.data])
  const detailQuery = useQuery({
    queryKey: ['withdrawal-detail', created?.kind, created?.data.id],
    queryFn: async () => created?.kind === 'TOKEN'
      ? { kind: 'TOKEN' as const, data: await api.tokenWithdrawal(created.data.id) }
      : { kind: 'ETH' as const, data: await api.withdrawal(created!.data.id) },
    enabled: Boolean(created?.data.id),
    refetchInterval: (query) => ['COMPLETED', 'FAILED'].includes(query.state.data?.data.status ?? '') ? false : 3_000,
  })
  const current = detailQuery.data ?? created

  useEffect(() => {
    setQuote(undefined)
    setPendingValues(undefined)
    setCreated(undefined)
    form.resetFields()
  }, [asset.symbol, form, userId])

  useEffect(() => {
    if (detailQuery.data && ['COMPLETED', 'FAILED'].includes(detailQuery.data.data.status)) {
      queryClient.invalidateQueries({ queryKey: ['balances', userId] })
      queryClient.invalidateQueries({ queryKey: ['wallet', userId] })
      queryClient.invalidateQueries({ queryKey: ['transactions'] })
      queryClient.invalidateQueries({ queryKey: ['withdrawals', userId] })
      queryClient.invalidateQueries({ queryKey: ['token-withdrawals', userId] })
    }
  }, [detailQuery.data, queryClient, userId])

  const quoteMutation = useMutation({
    mutationFn: async (values: WithdrawForm): Promise<Quote> => isToken
      ? { kind: 'TOKEN', data: await api.quoteTokenWithdrawal(userId, { to_address: values.to_address, amount: values.amount }) }
      : { kind: 'ETH', data: await api.quoteWithdrawal(userId, { to_address: values.to_address, amount_eth: values.amount }) },
    onSuccess: (data, values) => {
      setQuote(data)
      setPendingValues(values)
    },
  })
  const createMutation = useMutation({
    mutationFn: async (values: WithdrawForm): Promise<Created> => isToken
      ? { kind: 'TOKEN', data: await api.createTokenWithdrawal(userId, crypto.randomUUID(), { to_address: values.to_address, amount: values.amount }) }
      : { kind: 'ETH', data: await api.createWithdrawal(userId, crypto.randomUUID(), { to_address: values.to_address, amount_eth: values.amount }) },
    onSuccess: (data) => {
      setCreated(data)
      setPendingValues(undefined)
      queryClient.invalidateQueries({ queryKey: ['balances', userId] })
      queryClient.invalidateQueries({ queryKey: ['wallet', userId] })
    },
  })

  if (asset.network.startsWith('bitcoin-')) return <BitcoinWithdrawPanel userId={userId} />

  const requestQuote = async () => {
    const values = await form.validateFields()
    setQuote(undefined)
    quoteMutation.mutate(values)
  }

  const confirmWithdrawal = () => {
    if (!pendingValues || !quote) return
    const fee = quote.kind === 'ETH' ? quote.data.reserved_fee_eth : quote.data.estimated_gas_eth
    modal.confirm({
      title: `确认提交 ${asset.symbol} 提币`,
      content: (
        <div className="confirm-details">
          <p><span>到账地址</span><strong className="mono break-all">{pendingValues.to_address}</strong></p>
          <p><span>提币金额</span><strong>{pendingValues.amount} {asset.symbol}</strong></p>
          <p><span>{isToken ? '平台预计 Gas' : '最大网络费'}</span><strong>{fee} ETH</strong></p>
          {!isToken && <p><span>最大扣减</span><strong>{addETH(pendingValues.amount, fee)} ETH</strong></p>}
        </div>
      ),
      okText: '确认提币',
      cancelText: '取消',
      okButtonProps: { danger: true },
      onOk: () => createMutation.mutateAsync(pendingValues),
    })
  }

  if (current) {
    const item = current.data
    const amount = current.kind === 'TOKEN' ? current.data.amount : current.data.amount_eth
    const estimatedFee = current.kind === 'TOKEN' ? current.data.estimated_gas_eth : current.data.reserved_fee_eth
    const step = item.status === 'COMPLETED' ? 3 : item.status === 'FAILED' ? 1 : item.tx_hash ? 2 : 1
    return (
      <>
        <PageHeader title={`${asset.symbol} 提币进度`} />
        <section className="panel withdrawal-result">
          <Result
            status={item.status === 'FAILED' ? 'error' : item.status === 'COMPLETED' ? 'success' : 'info'}
            title={item.status === 'COMPLETED' ? '提币已完成' : item.status === 'FAILED' ? '提币失败' : '提币处理中'}
            subTitle={`${asset.symbol} 提币单 #${item.id}`}
          />
          <Steps current={step} status={item.status === 'FAILED' ? 'error' : 'process'} items={[{ title: '已创建' }, { title: '签名广播' }, { title: '链上确认' }, { title: '完成' }]} />
          <div className="withdrawal-detail-grid">
            <div><span>状态</span><StatusTag status={item.status} /></div>
            <div><span>金额</span><strong>{amount} {asset.symbol}</strong></div>
            <div><span>{isToken ? '预计平台 Gas' : '预留网络费'}</span><strong>{estimatedFee ? `${estimatedFee} ETH` : '--'}</strong></div>
            <div><span>实际网络费</span><strong>{item.actual_fee_eth ? `${item.actual_fee_eth} ETH` : '--'}</strong></div>
            <div><span>确认数</span><strong>{item.confirmations}</strong></div>
            <div><span>创建时间</span><strong>{formatDate(item.created_at)}</strong></div>
            <div className="wide"><span>交易哈希</span><HashLink hash={item.tx_hash} url={item.explorer_url} /></div>
          </div>
          {item.error_message && <Alert type="error" showIcon title={item.error_message} />}
          <div className="result-actions">
            <Button onClick={() => { setCreated(undefined); setQuote(undefined); form.resetFields() }}>发起新的提币</Button>
            {item.explorer_url && <Button icon={<ExternalLink size={16} />} href={item.explorer_url} target="_blank">区块浏览器</Button>}
          </div>
        </section>
      </>
    )
  }

  const fee = quote?.kind === 'ETH' ? quote.data.reserved_fee_eth : quote?.data.estimated_gas_eth
  const gasLimit = quote?.data.gas_limit
  return (
    <>
      <PageHeader title={`提币 ${asset.symbol}`} />
      <div className="withdraw-layout">
        <section className="panel withdraw-form-panel">
          <SectionHeader title="填写提币信息" />
          <QueryState loading={balancesQuery.isLoading} error={balancesQuery.error} retry={() => balancesQuery.refetch()} />
          {selectedBalance && <div className="available-line"><span>可用余额</span><strong>{formatETH(selectedBalance.available)} {asset.symbol}</strong></div>}
          <Form form={form} layout="vertical" requiredMark={false} onValuesChange={() => { setQuote(undefined); setPendingValues(undefined) }}>
            <Form.Item label="到账地址" name="to_address" rules={[{ required: true, message: '请输入到账地址' }, { pattern: /^0x[a-fA-F0-9]{40}$/, message: '请输入有效的 EVM 地址' }]}>
              <Input size="large" className="mono" placeholder="0x..." autoComplete="off" />
            </Form.Item>
            <Form.Item label="提币金额" name="amount" rules={[{ required: true, message: '请输入提币金额' }, { validator: (_, value) => !value || new RegExp(`^(?:0|[1-9]\\d*)(?:\\.\\d{1,${asset.decimals}})?$`).test(value) ? Promise.resolve() : Promise.reject(new Error(`请输入最多 ${asset.decimals} 位小数的有效金额`)) }]}>
              <Input size="large" suffix={asset.symbol} inputMode="decimal" placeholder={isToken ? '10' : '0.001'} autoComplete="off" />
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
                <p><span>提币金额</span><strong>{pendingValues?.amount} {asset.symbol}</strong></p>
                <p><span>Gas Limit</span><strong>{gasLimit?.toLocaleString()}</strong></p>
                <p><span>{isToken ? '平台预计 Gas' : '最大网络费'}</span><strong>{fee} ETH</strong></p>
              </div>
              <Alert type="info" showIcon title={isToken ? 'ERC-20 提币 Gas 由平台热钱包支付，不从用户 Token 余额中扣除。' : '实际网络费按链上执行结果结算，未使用的预留费用会返还余额。'} />
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
