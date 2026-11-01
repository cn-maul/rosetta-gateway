import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import {
  Button,
  Input,
  ListBox,
  ListBoxItem,
  Select,
  SelectIndicator,
  SelectPopover,
  SelectTrigger,
  SelectValue,
  Table,
  TextField,
  toast,
} from '@heroui/react'
import { api, isAdmin } from '../api'
import { effortLabel, effortTitle, effortTone } from '../effortTone'
import { fmtMoney, fmtNum, fmtSec, fmtSpeed, fmtTimeFull, fmtTokens, statusLabel } from '../fmt'
import { protocolLabel } from '../protocols'
import { parseRouteTrace } from '../routeTrace'
import type { AccessKey, UsageHistoryEntry } from '../types'
import { DataTable } from '../ui/DataTable'
import { StatusChip } from '../ui/StatusChip'
import { keyName, newInputTokens, statusTone, totalTimeTone, upstreamName, UsageDetailDrawer } from '../components/UsageDetailDrawer'
import { oneOfInt, readPref, writePref } from '../ui/usePersistedState'

/**
 * 调用日志（/history）。全部查询状态都在 URL 上：status/model/key/page/size。
 * 输入框只保留草稿；实际请求、分页与 CSV 始终来自 URL 中已应用的条件。
 */

const STATUSES = [
  { value: 'ok', label: '正常' },
  { value: 'error', label: '失败' },
  { value: 'truncated', label: '截断' },
  { value: 'overflow', label: '超限' },
  { value: 'canceled', label: '已取消' },
  { value: 'timeout', label: '网关超时' },
]

const SIZES = [20, 50, 100]

/** 首字超过 10s 红，5–10s 橙，5s 内绿；没有样本保持中性。 */
function timingTitle(h: UsageHistoryEntry): string {
  return `首字 ${h.ttfb_ms > 0 ? fmtSec(h.ttfb_ms) + ' 秒' : '无样本'} | 总耗时 ${h.latency_ms > 0 ? fmtSec(h.latency_ms) + ' 秒' : '无样本'}`
}

function upstreamNameTitle(h: UsageHistoryEntry): string {
  // 已删除的上游：名字是当时固化的，仍然显示，但在 title 里说明它已经不在了，
  // 免得有人照着这个名字去「上游服务」页找。
  const gone = h.provider_deleted ? '（该上游已被删除）' : ''
  const idPart = h.provider_id ? `（provider_id: ${h.provider_id}）` : ''
  if (h.provider_name) return h.provider_name + idPart + gone
  if (h.provider_id) return `旧记录未固化上游名称，provider_id: ${h.provider_id}` + gone
  return '这条记录没有归因到具体上游'
}

/** 速度格：tps = 0 表示没有可计算样本（上游未报 usage / 错误请求），
 * 不是「速度为零」。所以显示 —，不能把 0 直接印出来。 */
function speedText(h: UsageHistoryEntry): string {
  return h.tps > 0 ? fmtSpeed(h.tps) + ' t/s' : '—'
}

function statusHint(h: UsageHistoryEntry): string {
  const parts = [statusLabel(h.status)]
  if (h.http_status > 0) parts.push('HTTP ' + h.http_status)
  if (h.error_code) parts.push(h.error_code)
  return parts.join(' · ')
}

function tokenTitle(h: UsageHistoryEntry): string {
  return (
    '新增输入 ' + fmtNum(newInputTokens(h)) +
    ' · 输出 ' + fmtNum(h.output_tokens) +
    ' · 缓存命中 ' + fmtNum(h.cached_tokens) +
    ' · 缓存写入 ' + fmtNum(h.cached_creation_tokens) +
    ' · 推理 ' + fmtNum(h.reasoning_tokens) +
    ' · 总计 ' + fmtNum(h.total_tokens)
  )
}

function traceNodeTone(step: ReturnType<typeof parseRouteTrace>[number]): string {
  if (step.outcome === 'ok') return 'text-success'
  if (step.outcome === 'skip') return 'text-muted'
  return 'text-danger'
}

function PathFlow({ entry }: { entry: UsageHistoryEntry }) {
  const steps = useMemo(() => parseRouteTrace(entry.route_trace), [entry])
  if (!steps.length) return null
  return (
    <span className="flex flex-wrap items-center gap-1.5">
      {steps.map((step, i) => (
        <span key={i} className="flex items-center gap-1.5">
          {i > 0 ? <span aria-hidden="true" className="text-muted">→</span> : null}
          <span className={traceNodeTone(step)}>{step.parsed ? step.provider : '记录异常'}</span>
        </span>
      ))}
    </span>
  )
}

export function UsageDetail({ days }: { days: number }) {
  // 普通用户看的是「我自己调了哪些路由」，后台怎么转发跟他的排障无关：
  // 上游那一列整列不渲染，模型只显示路由名（public_model），不露背后实际模型。
  const admin = isAdmin()
  const [searchParams, setSearchParams] = useSearchParams()
  const [rows, setRows] = useState<UsageHistoryEntry[]>([])
  const [total, setTotal] = useState(0)
  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')
  const [keys, setKeys] = useState<AccessKey[]>([])
  const [keyError, setKeyError] = useState('')
  const [selected, setSelected] = useState<UsageHistoryEntry | null>(null)
  const [exporting, setExporting] = useState(false)
  const [exportNotice, setExportNotice] = useState('')
  const [loadedPage, setLoadedPage] = useState(1)
  const [loadedDays, setLoadedDays] = useState(days)
  const [fStatus, setFStatus] = useState('')
  const [fModel, setFModel] = useState('')
  const [fKey, setFKey] = useState('')

  const queryText = (key: string) => searchParams.get(key) ?? ''
  const request = useMemo(() => {
    const status = queryText('status')
    const size = Number(queryText('size'))
    return {
      status: STATUSES.some((s) => s.value === status) ? status : '',
      model: queryText('model').trim(),
      keyId: queryText('key'),
      page: Math.max(1, Math.min(1000000, Math.floor(Number(queryText('page'))) || 1)),
      // 每页条数没写在 URL 上时用上次的选择（URL 带 size 的分享链接照旧优先）。
      size: SIZES.includes(size) ? size : readPref('history.size', 20, oneOfInt(SIZES)),
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [searchParams])

  const totalPages = Math.max(1, Math.ceil(total / request.size))
  const reqSeq = useRef(0)

  // 输入框保留草稿；URL 条件变化（含外部跳转）时把草稿拉回与 URL 一致。
  useEffect(() => {
    setFStatus(request.status)
    setFModel(request.model)
    setFKey(request.keyId)
  }, [request.status, request.model, request.keyId])

  const pushQuery = useCallback(
    (patch: Record<string, string | undefined>) => {
      const sp = new URLSearchParams(searchParams)
      for (const [k, v] of Object.entries(patch)) {
        if (v === undefined || v === '') sp.delete(k)
        else sp.set(k, v)
      }
      setSearchParams(sp)
    },
    [searchParams, setSearchParams],
  )

  const filters = (status: string, model: string, keyId: string) => ({
    status: status || undefined,
    model: model || undefined,
    keyId: keyId || undefined,
  })

  const load = useCallback(async () => {
    const seq = ++reqSeq.current
    const r = request
    setLoading(true)
    setErr('')
    try {
      const result = await api.usageHistory(days, r.size, (r.page - 1) * r.size, filters(r.status, r.model, r.keyId))
      if (seq !== reqSeq.current) return
      const last = Math.max(1, Math.ceil(result.total / r.size))
      if (r.page > last) {
        pushQuery({ page: last === 1 ? undefined : String(last) })
        return
      }
      setRows(result.records)
      setTotal(result.total)
      setLoadedPage(r.page)
      setLoadedDays(days)
    } catch (e) {
      if (seq === reqSeq.current && (e as { status?: number }).status !== 401) {
        setErr('调用日志加载失败：' + (e as Error).message)
      }
    } finally {
      if (seq === reqSeq.current) setLoading(false)
    }
  }, [days, request, pushQuery])

  useEffect(() => {
    void load()
    return () => {
      ++reqSeq.current
    }
  }, [load])

  const applyFilters = useCallback(
    (override?: { status?: string; model?: string; key?: string }) => {
      const status = override?.status ?? fStatus
      const model = (override?.model ?? fModel).trim()
      const key = override?.key ?? fKey
      const changed = status !== request.status || model !== request.model || key !== request.keyId
      if (!changed) {
        void load()
        return
      }
      pushQuery({ status: status || undefined, model: model || undefined, key: key || undefined, page: undefined })
    },
    [fStatus, fModel, fKey, request, load, pushQuery],
  )

  const onModelBlur = () => {
    if (fModel.trim() !== request.model) applyFilters()
  }

  const resetFilters = () => {
    setFStatus('')
    setFModel('')
    setFKey('')
    applyFilters({ status: '', model: '', key: '' })
  }

  const go = (page: number) => {
    if (loading || page < 1 || page > totalPages) return
    pushQuery({ page: page === 1 ? undefined : String(page) })
  }

  const loadKeys = useCallback(async () => {
    setKeyError('')
    try {
      setKeys(await api.keys())
    } catch {
      setKeyError('密钥筛选暂不可用')
    }
  }, [])

  useEffect(() => {
    void loadKeys()
  }, [loadKeys])

  const exportCSV = async () => {
    setExporting(true)
    try {
      // 草稿还没应用时先写回 URL（分页会随之清掉），CSV 始终按草稿条件导。
      applyFilters()
      const result = await api.exportUsageCSV(days, filters(fStatus, fModel.trim(), fKey))
      if (result.truncated) {
        setExportNotice(
          (result.total === undefined ? '' : '共 ' + fmtNum(result.total) + ' 条，') +
            '本次仅导出最近 1,000 条。请缩小时间范围或增加筛选后重新导出。',
        )
      } else {
        setExportNotice('')
        toast.success('CSV 已开始下载')
      }
    } catch (e) {
      if ((e as { status?: number }).status !== 401) toast.danger('导出失败：' + (e as Error).message)
    } finally {
      setExporting(false)
    }
  }

  const errText = err + (rows.length ? '（近 ' + loadedDays + ' 天，第 ' + loadedPage + ' 页）' : '')

  const toolbar = (
    <>
      <div className="mb-4 flex flex-wrap items-center gap-2.5">
        <Select
          aria-label="请求状态"
          className="w-[130px]"
          selectedKey={fStatus}
          onSelectionChange={(key) => {
            const v = key === null ? '' : String(key)
            setFStatus(v)
            applyFilters({ status: v })
          }}
        >
          <SelectTrigger>
            <SelectValue />
            <SelectIndicator />
          </SelectTrigger>
          <SelectPopover>
            <ListBox aria-label="请求状态">
              <ListBoxItem id="" className="text-muted">全部状态</ListBoxItem>
              {STATUSES.map((s) => (
                <ListBoxItem key={s.value} id={s.value}>{s.label}</ListBoxItem>
              ))}
            </ListBox>
          </SelectPopover>
        </Select>
        <TextField aria-label="公开模型名" value={fModel} onChange={setFModel} className="w-[230px]">
          <Input
            placeholder="公开模型名（精确匹配）"
            className="font-mono"
            autoComplete="off"
            onBlur={onModelBlur}
            onKeyDown={(e) => {
              if (e.key === 'Enter') onModelBlur()
            }}
          />
        </TextField>
        <Select
          aria-label="调用密钥"
          className="w-[170px]"
          isDisabled={!!keyError}
          selectedKey={fKey}
          onSelectionChange={(key) => {
            const v = key === null ? '' : String(key)
            setFKey(v)
            applyFilters({ key: v })
          }}
        >
          <SelectTrigger>
            <SelectValue />
            <SelectIndicator />
          </SelectTrigger>
          <SelectPopover>
            <ListBox aria-label="调用密钥">
              <ListBoxItem id="" className="text-muted">全部密钥</ListBoxItem>
              {keys.map((k) => (
                <ListBoxItem key={k.id} id={k.id}>{k.name || k.key_prefix}</ListBoxItem>
              ))}
            </ListBox>
          </SelectPopover>
        </Select>
        <Button variant="secondary" size="sm" onPress={() => applyFilters()}>应用</Button>
        <Button variant="ghost" size="sm" onPress={resetFilters}>重置</Button>
      </div>
      <div className="mb-4 flex flex-wrap items-center justify-between gap-3 text-xs text-muted">
        <span>按时间倒序 · 共 {fmtNum(total)} 条</span>
        <div className="flex items-center gap-2.5">
          <Select
            aria-label="每页条数"
            className="w-[110px]"
            selectedKey={String(request.size)}
            onSelectionChange={(key) => {
              if (key !== null) {
                writePref('history.size', Number(key))
                pushQuery({ size: String(key), page: undefined })
              }
            }}
          >
            <SelectTrigger>
              <SelectValue />
              <SelectIndicator />
            </SelectTrigger>
            <SelectPopover>
              <ListBox aria-label="每页条数">
                {SIZES.map((n) => (
                  <ListBoxItem key={n} id={String(n)}>{n} 条/页</ListBoxItem>
                ))}
              </ListBox>
            </SelectPopover>
          </Select>
          <Button variant="secondary" size="sm" isDisabled={exporting || loading} onPress={() => void exportCSV()}>
            {exporting ? '导出中…' : '导出 CSV'}
          </Button>
          <Button variant="secondary" size="sm" isDisabled={loading} onPress={() => void load()}>刷新</Button>
        </div>
      </div>
      {keyError ? (
        <p className="mb-4 flex items-center gap-2 rounded-lg bg-warning-soft px-3.5 py-2 text-xs text-warning-foreground" role="status">
          {keyError}
          <Button variant="tertiary" size="sm" onPress={() => void loadKeys()}>重试</Button>
        </p>
      ) : null}
      {exportNotice ? (
        <p className="mb-4 rounded-lg bg-warning-soft px-3.5 py-2 text-xs text-warning-foreground" role="alert">
          {exportNotice}
        </p>
      ) : null}
    </>
  )

  const footer = (
    <div className="flex items-center justify-center gap-2 py-2">
      <Button variant="secondary" size="sm" isDisabled={request.page <= 1 || loading} onPress={() => go(1)}>首页</Button>
      <Button variant="secondary" size="sm" isDisabled={request.page <= 1 || loading} onPress={() => go(request.page - 1)}>上一页</Button>
      <span className="text-xs tabular-nums text-muted">第 {request.page} / {totalPages} 页</span>
      <Button variant="secondary" size="sm" isDisabled={request.page >= totalPages || loading} onPress={() => go(request.page + 1)}>下一页</Button>
      <Button variant="secondary" size="sm" isDisabled={request.page >= totalPages || loading} onPress={() => go(totalPages)}>末页</Button>
    </div>
  )

  return (
    <div>
      <DataTable
        label="调用日志"
        loading={loading}
        error={err ? errText : ''}
        hasData={rows.length > 0}
        empty={!rows.length}
        emptyText="当前条件下没有调用记录"
        onRetry={() => void load()}
        toolbar={toolbar}
        footer={footer}
      >
        <Table aria-label="调用日志">
          <Table.ScrollContainer>
            <Table.Content className="min-w-[1490px] text-[13px]">
              <Table.Header>
                <Table.Column isRowHeader>时间</Table.Column>
                <Table.Column>模式</Table.Column>
                <Table.Column>协议</Table.Column>
                <Table.Column>请求 IP</Table.Column>
                {admin ? <Table.Column>上游</Table.Column> : null}
                <Table.Column>模型</Table.Column>
                <Table.Column>密钥</Table.Column>
                <Table.Column>Token 用量</Table.Column>
                <Table.Column>速度</Table.Column>
                <Table.Column>时间</Table.Column>
                <Table.Column>费用</Table.Column>
                <Table.Column>状态</Table.Column>
              </Table.Header>
              <Table.Body>
                {rows.map((h, i) => (
                  <Table.Row key={h.ts + ':' + i}>
                    <Table.Cell>
                      <span className="font-mono">{fmtTimeFull(h.ts)}</span>
                    </Table.Cell>
                    <Table.Cell>
                      <span className="flex flex-col items-start gap-1">
                        <StatusChip tone={h.stream ? 'info' : 'dim'}>{h.stream ? '流式' : '非流式'}</StatusChip>
                        {h.effort ? (
                          <StatusChip tone={effortTone(h.effort)} title={effortTitle(h.effort)}>
                            {effortLabel(h.effort)}
                          </StatusChip>
                        ) : null}
                      </span>
                    </Table.Cell>
                    <Table.Cell>
                      {/* 「上游/下游」两个词占了大半格宽，而这一列真正要看的是两个协议名。
                          ↑ 上行（网关发给上游）、↓ 下行（客户端进来的），读屏走 sr-only。 */}
                      <span title={`上行协议：${protocolLabel(h.upstream_protocol, '未记录')}；下行协议：${protocolLabel(h.ingress_protocol, '未记录')}`} className="flex flex-col gap-0.5 text-[11.5px]">
                        <span className="flex items-center gap-1.5">
                          <span aria-hidden="true" className="text-muted">↑</span>
                          <span className="sr-only">上行</span>
                          <span className="font-mono">{protocolLabel(h.upstream_protocol, '未记录')}</span>
                        </span>
                        <span className="flex items-center gap-1.5">
                          <span aria-hidden="true" className="text-muted">↓</span>
                          <span className="sr-only">下行</span>
                          <span className="font-mono">{protocolLabel(h.ingress_protocol, '未记录')}</span>
                        </span>
                      </span>
                    </Table.Cell>
                    <Table.Cell>
                      <span className="font-mono" title={h.request_ip || '未记录'}>{h.request_ip || '—'}</span>
                    </Table.Cell>
                    {admin ? (
                      <Table.Cell>
                        {parseRouteTrace(h.route_trace).length ? (
                          <PathFlow entry={h} />
                        ) : (
                          <span title={upstreamNameTitle(h)} className="flex flex-col">
                            <span>{upstreamName(h)}</span>
                            {!h.provider_name && h.provider_id ? (
                              <span className="text-[11px] text-muted">名称未固化</span>
                            ) : h.provider_deleted ? (
                              <span className="text-[11px] text-warning">上游已删除</span>
                            ) : null}
                          </span>
                        )}
                      </Table.Cell>
                    ) : null}
                    <Table.Cell>
                      <span className="flex flex-col">
                        <span className="font-mono" title={h.public_model || '—'}>{h.public_model || '—'}</span>
                        {admin ? (
                          <span className="font-mono text-[11.5px] text-muted" title={h.upstream_model || '实际模型未记录'}>
                            {h.upstream_model || '实际模型未记录'}
                          </span>
                        ) : null}
                      </span>
                    </Table.Cell>
                    <Table.Cell>
                      {/* 名字是落库时固化的：密钥删掉之后仍显示当时的名字，
                          加「已删除 · 」前缀；只有升级前的旧记录才回退到 id。 */}
                      <span title={keyName(h)}>{keyName(h)}</span>
                    </Table.Cell>
                    <Table.Cell>
                      {/* 箭头代替「新增输入 / 输出」两个词：这一列是表里最挤的一格，
                          文字标签会把数字挤到换行。↑ 输入（未命中缓存的新增部分），
                          ↓ 输出；完整口径在 title 里，读屏走 sr-only 文本。
                          缓存命中单独一行，它是「输入里有多少是缓存命中」的注脚。 */}
                      <span title={tokenTitle(h)} className="flex flex-col">
                        <span className="tabular-nums">
                          <span aria-hidden="true" className="text-muted">↑</span>
                          <span className="sr-only">输入</span> {fmtTokens(newInputTokens(h))}{' '}
                          <span aria-hidden="true" className="text-muted">↓</span>
                          <span className="sr-only">输出</span> {fmtTokens(h.output_tokens)}
                        </span>
                        <span className="text-[11px] text-muted">缓存命中 {fmtTokens(h.cached_tokens)}</span>
                      </span>
                    </Table.Cell>
                    <Table.Cell>{speedText(h)}</Table.Cell>
                    <Table.Cell>
                      <span title={timingTitle(h)} className="flex items-center gap-1.5 tabular-nums">
                        <span className={h.ttfb_ms <= 0 ? 'text-muted' : h.ttfb_ms <= 5000 ? 'text-success' : h.ttfb_ms <= 10000 ? 'text-warning' : 'text-danger'}>
                          {h.ttfb_ms > 0 ? fmtSec(h.ttfb_ms) : '—'}
                        </span>
                        <span className="text-muted">|</span>
                        <span className={totalTimeTone(h)}>
                          {h.latency_ms > 0 ? fmtSec(h.latency_ms) : '—'}
                        </span>
                      </span>
                    </Table.Cell>
                    <Table.Cell>¥{fmtMoney(h.cost)}</Table.Cell>
                    <Table.Cell>
                      {/* 抽屉只讲失败原因，成功的行没什么可展开的：给个按钮点开
                          只会得到一句「这条成功了」。所以只有非 ok 才是按钮。 */}
                      {h.status === 'ok' ? (
                        <span title={statusHint(h)} className="flex items-center gap-2">
                          <StatusChip tone={statusTone(h.status)}>{statusLabel(h.status)}</StatusChip>
                          {h.http_status > 0 ? <span className="text-[11px] text-muted">HTTP {h.http_status}</span> : null}
                        </span>
                      ) : (
                        <Button
                          variant="ghost"
                          size="sm"
                          aria-label={'查看' + statusLabel(h.status) + '错误详情'}
                          onPress={() => setSelected(h)}
                        >
                          <span title={statusHint(h) + ' · 点击查看错误详情'} className="flex items-center gap-2">
                            <StatusChip tone={statusTone(h.status)}>{statusLabel(h.status)}</StatusChip>
                            {h.http_status > 0 ? <span className="text-[11px] text-muted">HTTP {h.http_status}</span> : null}
                          </span>
                        </Button>
                      )}
                    </Table.Cell>
                  </Table.Row>
                ))}
              </Table.Body>
            </Table.Content>
          </Table.ScrollContainer>
        </Table>
      </DataTable>
      <UsageDetailDrawer entry={selected} onClose={() => setSelected(null)} />
    </div>
  )
}
