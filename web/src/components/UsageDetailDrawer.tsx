import { useMemo, useState } from 'react'
import { Button, Disclosure, DisclosureBody, DisclosureContent, DisclosureHeading, DisclosureIndicator, DisclosureTrigger } from '@heroui/react'
import { copyText, fmtMoney, fmtNum, fmtSec, fmtSpeed, fmtTimeMs, statusLabel } from '../fmt'
import { protocolLabel } from '../protocols'
import { parseRouteTrace } from '../routeTrace'
import type { UsageHistoryEntry } from '../types'
import { AppDrawer } from '../ui/AppDrawer'
import { StatusChip } from '../ui/StatusChip'

/**
 * 请求详情抽屉（语义对齐旧 DetailDrawer.vue / UsageDetail.vue 内嵌段）：
 * 请求事实、转发决策轨迹、错误详情三段。错误正文分「上游说明」与
 * 「完整原始响应」两块展示，原始 JSON 自动 pretty 化。
 */

/** 状态徽章的语义色。ok 绿；error/timeout 红；截断/超限橙；取消灰。 */
export function statusTone(status: string): 'ok' | 'err' | 'warn' | 'dim' {
  switch (status) {
    case 'ok':
      return 'ok'
    case 'error':
    case 'timeout':
      return 'err'
    case 'truncated':
    case 'overflow':
      return 'warn'
    default:
      return 'dim'
  }
}

function modeLabel(h: UsageHistoryEntry): string {
  return h.stream ? '流式' : '非流式'
}

/** 首字超过 10s 红，5–10s 橙，5s 内绿；没有样本保持中性。 */
function firstTokenTone(h: UsageHistoryEntry): string {
  if (h.ttfb_ms <= 0) return 'text-muted'
  if (h.ttfb_ms <= 5000) return 'text-success'
  if (h.ttfb_ms <= 10000) return 'text-warning'
  return 'text-danger'
}

/** 总耗时超过 30s 红，10–30s 橙，10s 内绿；没有样本保持中性。 */
export function totalTimeTone(h: UsageHistoryEntry): string {
  if (h.latency_ms <= 0) return 'text-muted'
  if (h.latency_ms <= 10000) return 'text-success'
  if (h.latency_ms <= 30000) return 'text-warning'
  return 'text-danger'
}

/**
 * 统一 Usage 的 input_tokens 是输入总量，包含缓存命中和缓存写入。
 * 「新增输入」只展示本次真正新增处理、未命中缓存的输入 token。
 */
export function newInputTokens(h: UsageHistoryEntry): number {
  return Math.max(0, h.input_tokens - h.cached_tokens - h.cached_creation_tokens)
}

/**
 * 实际接单的上游展示名。provider_name 是落库时固化的历史值；旧记录没有
 * 这个字段，回退到 provider_id 并明确标注，避免让人误以为那就是名字。
 */
export function upstreamName(h: UsageHistoryEntry): string {
  return h.provider_name || h.provider_id || '上游未记录'
}

/** 错误详情分两段：上游那句 message 与完整 raw body。老记录可能只有一段
 * 裸文本/裸 JSON，这里统一识别，交给模板分区渲染。 */
function errorView(detail: string | undefined) {
  const value = detail?.trim() ?? ''
  if (!value) return { message: '', rawPretty: '' }
  let message = ''
  let raw = ''
  if (value.startsWith('message: ')) {
    const rawAt = value.indexOf('\nraw: ')
    if (rawAt >= 0) {
      message = value.slice('message: '.length, rawAt)
      raw = value.slice(rawAt + '\nraw: '.length)
    } else {
      message = value.slice('message: '.length)
    }
  } else if (value.startsWith('raw: ')) {
    raw = value.slice('raw: '.length)
  } else {
    message = value
  }
  const looksJSON = message.trimStart().startsWith('{') || message.trimStart().startsWith('[')
  if (!raw && looksJSON) {
    raw = message
    message = ''
  }
  let rawPretty = raw
  if (raw) {
    try {
      rawPretty = JSON.stringify(JSON.parse(raw), null, 2)
    } catch {
      /* 不是合法 JSON 就保留原文 */
    }
  }
  return { message, rawPretty }
}

function Fact({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="grid grid-cols-[120px_minmax(0,1fr)] gap-4 border-b border-separator py-3">
      <dt className="text-xs text-muted">{label}</dt>
      <dd className="text-[13px] break-words">{children}</dd>
    </div>
  )
}

function DetailContent({ entry }: { entry: UsageHistoryEntry }) {
  const parsedTrace = useMemo(() => parseRouteTrace(entry.route_trace), [entry])
  const ev = useMemo(() => errorView(entry.error_detail), [entry])
  const [copyNotice, setCopyNotice] = useState('')

  const copyError = async () => {
    const text = entry.error_detail ?? ''
    if (!text) return
    setCopyNotice((await copyText(text)) ? '已复制' : '复制失败，请手动选择文本')
    setTimeout(() => setCopyNotice(''), 1800)
  }

  return (
    <>
      <div className="flex flex-wrap items-center justify-between gap-3 text-xs text-muted">
        <StatusChip tone={statusTone(entry.status)}>{statusLabel(entry.status)}</StatusChip>
        <span>
          {fmtTimeMs(entry.ts)} · {modeLabel(entry)} · {protocolLabel(entry.ingress_protocol)} →{' '}
          {protocolLabel(entry.upstream_protocol)}
        </span>
      </div>

      <dl className="mt-5">
        <Fact label="请求 ID">
          <span className="font-mono">{entry.request_id || '未记录'}</span>
        </Fact>
        <Fact label="请求 IP">
          <span className="font-mono">{entry.request_ip || '未记录'}</span>
        </Fact>
        <Fact label="上游">
          <span className="font-semibold">{upstreamName(entry)}</span>
          {entry.provider_id ? <span className="mt-1.5 block font-mono text-[11px] text-muted">provider_id: {entry.provider_id}</span> : null}
        </Fact>
        <Fact label="上游协议 / 入站协议">
          {protocolLabel(entry.upstream_protocol, '未记录')} / {protocolLabel(entry.ingress_protocol, '未记录')}
          <span className="mt-1.5 block text-[11px] text-muted">上行是网关连接上游使用的协议；下行是客户端调用网关使用的协议。</span>
        </Fact>
        <Fact label="公开模型名">
          <span className="font-mono">{entry.public_model || '—'}</span>
        </Fact>
        <Fact label="实际模型">
          <span className="font-mono">{entry.upstream_model || '未记录'}</span>
        </Fact>
        <Fact label="调用密钥">{entry.key_name || entry.key_id || '未记录'}</Fact>
        <Fact label="Token 用量">
          新增输入 {fmtNum(newInputTokens(entry))} · 输出 {fmtNum(entry.output_tokens)} · 总计 {fmtNum(entry.total_tokens)}
          <span className="mt-1.5 block text-[11px] text-muted">新增输入 = 输入总量 - 缓存命中 - 缓存写入</span>
        </Fact>
        <Fact label="缓存命中 / 写入 / 推理">
          {fmtNum(entry.cached_tokens)} / {fmtNum(entry.cached_creation_tokens)} / {fmtNum(entry.reasoning_tokens)} Tokens
        </Fact>
        <Fact label="记录费用">
          ¥{fmtMoney(entry.cost)}
          <span className="mt-1.5 block text-[11px] text-muted">按调用当时单价固化；0 元不能单独说明是否配价</span>
        </Fact>
        <Fact label="首字 / 总耗时">
          <span className="flex flex-wrap gap-4 tabular-nums">
            <span className={firstTokenTone(entry)}>首字 {entry.ttfb_ms > 0 ? fmtSec(entry.ttfb_ms) + 's' : '—'}</span>
            <span className={totalTimeTone(entry)}>总耗时 {entry.latency_ms > 0 ? fmtSec(entry.latency_ms) + 's' : '—'}</span>
          </span>
        </Fact>
        <Fact label="输出速度">{entry.tps > 0 ? fmtSpeed(entry.tps) + ' tok/s' : '无可计算样本'}</Fact>
        {entry.http_status > 0 ? (
          <Fact label="HTTP 状态">{entry.http_status}</Fact>
        ) : null}
        {entry.error_code ? (
          <Fact label="错误码">
            <span className="font-mono">{entry.error_code}</span>
          </Fact>
        ) : null}
      </dl>

      <section className="mt-6">
        <div className="mb-3.5 flex flex-wrap items-center justify-between gap-3">
          <h2 className="text-sm font-semibold">转发决策轨迹</h2>
          {parsedTrace.length > 1 ? <span className="text-[11.5px] text-muted">共 {parsedTrace.length} 个决策</span> : null}
        </div>
        {parsedTrace.length ? (
          <>
            <p className="rounded-lg bg-background-tertiary px-3.5 py-2 text-xs text-muted">
              编号表示本次请求的决策顺序，包含跳过的目标。配置位置与实际调用次数可能不同。
            </p>
            <ol className="mt-3 space-y-2.5">
              {parsedTrace.map((step, i) => (
                <li key={i} className="flex gap-3 rounded-xl bg-background-tertiary px-3 py-3 text-xs">
                  <span className="grid h-6 w-6 flex-none place-items-center rounded-full bg-background-secondary text-muted">
                    {step.attempt ?? i + 1}
                  </span>
                  {step.parsed ? (
                    <div className="min-w-0 flex-1">
                      <div className="flex flex-wrap items-center gap-2.5 break-all">
                        <strong>{step.provider}</strong>
                        <StatusChip tone={step.outcome === 'ok' ? 'ok' : step.outcome === 'skip' ? 'dim' : 'err'}>
                          {step.outcomeLabel}
                        </StatusChip>
                      </div>
                      <div className="mt-1.5 break-all font-mono">{step.model}</div>
                      {step.reasonLabel ? <div className="mt-2 leading-relaxed">{step.reasonLabel}</div> : null}
                      {step.explanation ? <p className="mt-1.5 text-xs leading-relaxed text-muted">{step.explanation}</p> : null}
                      <Disclosure className="mt-2 text-muted">
                        <DisclosureHeading className="text-xs">
                          <DisclosureTrigger className="text-xs">原始记录</DisclosureTrigger>
                          <DisclosureIndicator />
                        </DisclosureHeading>
                        <DisclosureContent>
                          <DisclosureBody>
                            <code className="mt-1.5 block break-all whitespace-pre-wrap font-mono">{step.raw}</code>
                          </DisclosureBody>
                        </DisclosureContent>
                      </Disclosure>
                    </div>
                  ) : (
                    <div className="min-w-0 flex-1">
                      <p className="text-xs leading-relaxed text-muted">记录格式无法解析，保留原文供排查。</p>
                      <code className="mt-1.5 block break-all whitespace-pre-wrap font-mono">{step.raw}</code>
                    </div>
                  )}
                </li>
              ))}
            </ol>
          </>
        ) : (
          <p className="rounded-lg bg-background-tertiary px-3.5 py-2 text-xs text-muted">
            此记录未提供转发轨迹，无法从中还原完整尝试过程。
          </p>
        )}
      </section>

      {entry.status !== 'ok' || entry.error_detail || entry.error_code ? (
        <section className="mt-6">
          <div className="mb-3.5 flex flex-wrap items-center justify-between gap-3">
            <h2 className="text-sm font-semibold">错误详情</h2>
            {entry.error_detail ? (
              <Button variant="ghost" size="sm" onPress={() => void copyError()}>
                {copyNotice || '复制完整错误'}
              </Button>
            ) : null}
          </div>
          <div className="grid grid-cols-3 gap-2.5">
            <div className="min-w-0 rounded-lg bg-background-tertiary px-3 py-2.5">
              <span className="mb-1 block text-[10.5px] text-muted">HTTP</span>
              <strong className="break-all">{entry.http_status || '—'}</strong>
            </div>
            <div className="min-w-0 rounded-lg bg-background-tertiary px-3 py-2.5">
              <span className="mb-1 block text-[10.5px] text-muted">错误码</span>
              <code className="break-all font-mono">{entry.error_code || '未记录'}</code>
            </div>
            <div className="min-w-0 rounded-lg bg-background-tertiary px-3 py-2.5">
              <span className="mb-1 block text-[10.5px] text-muted">终端</span>
              <strong className="break-all">{parsedTrace.at(-1)?.provider || upstreamName(entry)}</strong>
            </div>
          </div>
          {ev.message ? (
            <div className="mt-3.5">
              <h3 className="mb-2 text-xs font-semibold text-muted">上游返回的说明</h3>
              <pre className="rounded-xl bg-danger-soft px-3.5 py-3.5 text-xs leading-relaxed whitespace-pre-wrap break-words text-danger-soft-foreground">
                {ev.message}
              </pre>
            </div>
          ) : null}
          {ev.rawPretty ? (
            <div className="mt-3.5">
              <h3 className="mb-2 text-xs font-semibold text-muted">完整原始响应</h3>
              <pre className="max-h-[52vh] overflow-auto rounded-xl bg-background-tertiary px-3.5 py-3.5 font-mono text-xs leading-relaxed whitespace-pre-wrap break-words">
                {ev.rawPretty}
              </pre>
            </div>
          ) : null}
          {!ev.message && !ev.rawPretty ? (
            <p className="mt-3 rounded-lg bg-background-tertiary px-3.5 py-2 text-xs text-muted">
              没有可展示的上游正文：这类失败可能在连接阶段发生，或上游未返回错误体。上面的错误码与转发轨迹仍可用于定位。
            </p>
          ) : null}
        </section>
      ) : null}
    </>
  )
}

export function UsageDetailDrawer({ entry, onClose }: { entry: UsageHistoryEntry | null; onClose: () => void }) {
  return (
    <AppDrawer open={!!entry} onClose={onClose} title="请求详情">
      {entry ? <DetailContent entry={entry} /> : null}
    </AppDrawer>
  )
}
