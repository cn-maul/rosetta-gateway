import { useMemo, useState } from 'react'
import { Button } from '@heroui/react'
import { copyText, fmtTimeMs, statusLabel } from '../fmt'
import type { UsageHistoryEntry } from '../types'
import { AppDrawer } from '../ui/AppDrawer'
import { StatusChip } from '../ui/StatusChip'

/**
 * 请求详情抽屉：**只承载「表格里看不到的东西」——失败原因**。
 *
 * 列表已经把时间、协议、上游、模型、密钥、Token、速度、耗时、费用、状态都印在
 * 行里了，抽屉再抄一遍只是让人在两处对照同一批数字。排查失败要的是外面没有的
 * 三样：上游那句原话、完整原始响应、以及这单到底停在哪个上游。
 *
 * 抽屉的宽度/定位见 ui/AppDrawer.tsx（宽度只能走 style，className 会被插槽丢弃）。
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
 * 实际接单的上游展示名。
 *
 * provider_name 是落库时固化的历史值 —— 上游被删掉之后它仍然在，这正是
 * 我们要的：调用历史回答「当时是谁接的」，而不是「今天这个 id 指着谁」。
 * 所以顺序是：有名字就用名字，其次才是 id（旧记录没固化名字）。
 *
 * 上游已被删除时加「已删除 · 」前缀：不加前缀的话，一个已经不存在的上游
 * 与一个活着的上游在列表里长得一模一样，排障时会白找一圈。
 */
export function upstreamName(h: UsageHistoryEntry): string {
  const name = h.provider_name || h.provider_id || '上游未记录'
  return h.provider_deleted ? `已删除 · ${name}` : name
}

/**
 * 密钥的展示名。key_name 同样是落库时固化的（2026-11-01 新增列），
 * 密钥删掉后仍显示当时的名字，而不是那串 sha256 id。
 */
export function keyName(h: UsageHistoryEntry): string {
  const name = h.key_name || h.key_id || '—'
  return h.key_deleted ? `已删除 · ${name}` : name
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

function Scale({ label, children, tone }: { label: string; children: React.ReactNode; tone?: string }) {
  return (
    <div className="min-w-0 rounded-lg bg-background-tertiary px-3 py-2.5">
      <span className="mb-1 block text-[10.5px] text-muted">{label}</span>
      <strong className={'block break-all ' + (tone ?? '')}>{children}</strong>
    </div>
  )
}

function DetailContent({ entry }: { entry: UsageHistoryEntry }) {
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
      {/* 一行定位信息：哪一条请求、什么状态。表格里已经有的字段（模型、密钥、
          Token、耗时…）不再重复；request_id 是这张表唯一没印的，留着方便对日志。 */}
      <div className="flex flex-wrap items-center justify-between gap-3 text-xs text-muted">
        <StatusChip tone={statusTone(entry.status)}>{statusLabel(entry.status)}</StatusChip>
        <span className="font-mono">
          {fmtTimeMs(entry.ts)}
          {entry.request_id ? ' · ' + entry.request_id : ''}
        </span>
      </div>

      <div className="mt-4 grid grid-cols-3 gap-2.5">
        <Scale label="HTTP" tone={entry.http_status >= 400 || entry.http_status === 0 ? 'text-danger' : undefined}>
          {entry.http_status || '—'}
        </Scale>
        <Scale label="错误码">
          <code className="break-all font-mono">{entry.error_code || '未记录'}</code>
        </Scale>
        <Scale label="停在哪">{upstreamName(entry)}</Scale>
      </div>

      {entry.error_detail ? (
        <div className="mt-3 flex justify-end">
          <Button variant="ghost" size="sm" onPress={() => void copyError()}>
            {copyNotice || '复制完整错误'}
          </Button>
        </div>
      ) : null}

      {ev.message ? (
        <div className="mt-1">
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
        <p className="mt-3 rounded-lg bg-background-tertiary px-3.5 py-2 text-xs leading-relaxed text-muted">
          {entry.status === 'ok'
            ? '这条请求成功了，没有错误详情。耗时、Token 与费用都印在列表那一行里。'
            : entry.status === 'canceled'
              ? '这条请求被调用方取消，上游没有返回错误正文。'
              : '这条请求没有可展示的上游错误正文。失败可能发生在连接阶段（连不上、证书、DNS），或上游没回错误体；错误码与「停在哪」通常已足够定位。整条请求的耗时、Token 与费用在列表那一行里。'}
        </p>
      ) : null}
    </>
  )
}

export function UsageDetailDrawer({ entry, onClose }: { entry: UsageHistoryEntry | null; onClose: () => void }) {
  return (
    <AppDrawer open={!!entry} onClose={onClose} title="错误详情">
      {entry ? <DetailContent entry={entry} /> : null}
    </AppDrawer>
  )
}
