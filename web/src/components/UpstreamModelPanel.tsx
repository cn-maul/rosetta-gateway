import { Button, Table } from '@heroui/react'
import { DataTable } from '../ui/DataTable'
import { StatusChip, type StatusTone } from '../ui/StatusChip'
import { fmtSpeed, fmtSec, fmtPercent, fmtTokens } from '../fmt'
import { unitPrice } from '../modelPrice'
import { effortTone } from '../effortTone'
import type { UpstreamModel } from '../types'

/**
 * 上游模型面板：某个上游下声明的模型列表（详情页下半部分）。
 *
 * 与旧 UpstreamModelPanel.vue 相同的取舍：只搬展示，写操作
 * （测试/编辑/删除）由父组件持有 —— 父组件是唯一的写路径。
 *
 * 2026-10-31 改版（用户要求）：价格与「思考」各自独立成列、整表左对齐。
 * 原来价格与挡位挤在模型名下面当副行，一列里塞四行字，扫不动；
 * 拆列之后同一行内横着比价、竖着比挡位。
 */

/**
 * 一格里三个单价横排成「输入 | 输出 | 缓存命中」——
 * 顺序与编辑弹窗的字段顺序一致（输入、缓存命中、输出；缓存写入价 2026-10-31
 * 起不再暴露，未配置时按输入价计费）。
 * 未配置用「—」而不是「未配置」：一格里挤三个词会撑爆列宽，
 * 完整口径在 title 与编辑弹窗里。
 */
function priceCell(m: UpstreamModel) {
  const parts: { key: string; label: string; value: number }[] = [
    { key: 'in', label: '输入', value: m.price_input },
    { key: 'out', label: '输出', value: m.price_output },
    { key: 'hit', label: '缓存命中', value: m.price_cache_hit },
  ]
  return (
    <span
      className="flex items-center gap-1.5 text-xs whitespace-nowrap tabular-nums"
      title={parts.map((p) => `${p.label} ${unitPrice(p.value)}`).join(' · ')}
    >
      {parts.map((p, i) => (
        <span key={p.key} className="flex items-center gap-1.5">
          {i > 0 ? <span className="text-muted/60">|</span> : null}
          <span className={p.value > 0 ? undefined : 'text-muted'}>
            {p.value > 0 ? unitPrice(p.value) : '—'}
          </span>
        </span>
      ))}
    </span>
  )
}

/**
 * 「是否支持思考」与「有哪几档」必须一起显示：
 * 只显示挡位会让人以为「配了档位就等于支持思考」，而数据面在
 * 「不支持思考」时会直接把思考配置剥掉。
 *
 * supports_thinking 是三态（未配置/支持/不支持），三态都要能看出来 ——
 * 把「未配置」显示成「不支持」会让所有未配置的模型凭空失去思考能力。
 */
/**
 * 「上下文 / 最大输出」两格横排，单位按 128K / 1M 缩写。
 *
 * 0 表示未设置（落库时由「模型默认值」兜底，见 Settings 页），显示「—」
 * 而不是「0」：0 token 的上下文窗口没有意义，写出来会被误读成「这个模型
 * 用不了」。完整精确值放在 title 里 —— 缩写会丢掉 131072 这种细节，而
 * 排查「客户端报超长」时需要的正是精确数。
 */
function capacityCell(m: UpstreamModel) {
  const ctx = m.context_window > 0 ? fmtTokens(m.context_window) : '—'
  const out = m.max_output_tokens > 0 ? fmtTokens(m.max_output_tokens) : '—'
  const hint = [
    m.context_window > 0 ? `上下文 ${m.context_window.toLocaleString('en-US')} tokens` : '上下文：未设置（用默认值）',
    m.max_output_tokens > 0
      ? `最大输出 ${m.max_output_tokens.toLocaleString('en-US')} tokens`
      : '最大输出：未设置（用默认值）',
  ].join(' · ')
  return (
    <span
      className="flex items-center gap-1.5 text-xs whitespace-nowrap tabular-nums"
      title={hint}
    >
      <span className={m.context_window > 0 ? undefined : 'text-muted'}>{ctx}</span>
      <span className="text-muted/60">/</span>
      <span className={m.max_output_tokens > 0 ? undefined : 'text-muted'}>{out}</span>
    </span>
  )
}

function thinkingCell(m: UpstreamModel) {  const supported =
    m.supports_thinking === undefined
      ? { text: '未配置', tone: 'dim' as StatusTone, hint: '未配置：网关不干预，客户端维持既有行为' }
      : m.supports_thinking
        ? { text: '支持思考', tone: 'ok' as StatusTone, hint: undefined }
        : { text: '不支持', tone: 'err' as StatusTone, hint: undefined }

  return (
    <div className="flex min-w-0 flex-col items-start gap-1">
      <StatusChip tone={supported.tone} title={supported.hint}>
        {supported.text}
      </StatusChip>
      {m.effort_levels.length ? (
        <div className="flex flex-wrap items-center gap-1">
          {m.effort_levels.map((lvl) => (
            <StatusChip key={lvl} tone={effortTone(lvl)}>
              {lvl}
            </StatusChip>
          ))}
        </div>
      ) : (
        <span className="text-muted text-[11px]">未配置挡位</span>
      )}
      {m.unknown_effort_levels?.length ? (
        <div className="flex flex-wrap items-center gap-1">
          {m.unknown_effort_levels.map((lvl) => (
            <StatusChip key={lvl} tone="warn" title="网关不认识这个厂商私有挡位，会原样透传">
              {lvl}
            </StatusChip>
          ))}
        </div>
      ) : null}
    </div>
  )
}

interface UpstreamModelPanelProps {
  models: UpstreamModel[]
  /** 正在探测的 model id 集合。 */
  testing: Set<string>
  error?: string
  onRetry?: () => void
  onAdd: () => void
  onTest: (m: UpstreamModel) => void
  onEdit: (m: UpstreamModel) => void
  onRemove: (m: UpstreamModel) => void
}

export function UpstreamModelPanel({
  models,
  testing,
  error,
  onRetry,
  onAdd,
  onTest,
  onEdit,
  onRemove,
}: UpstreamModelPanelProps) {
  return (
    <section className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-sm font-semibold">上游模型（{models.length}）</h2>
        <Button variant="outline" size="sm" onPress={onAdd}>
          添加模型
        </Button>
      </div>

      <DataTable
        label="上游模型"
        error={error}
        hasData={models.length > 0}
        onRetry={onRetry}
        emptyText="无模型 —— 路由必须指向一个上游模型"
      >
        <Table aria-label="上游模型">
          <Table.ScrollContainer>
            <Table.Content className="min-w-[1120px]">
              {/* 整表左对齐（用户要求）：数字列也左对齐，与其余运维表一致。
                  表头里把价位顺序写进列名，省得每次都要对着数字猜哪格是哪个。 */}
              <Table.Header>
                <Table.Column isRowHeader>模型</Table.Column>
                <Table.Column>上下文 / 最大输出</Table.Column>
                <Table.Column>价格（元/1M） 输入 | 输出 | 缓存命中</Table.Column>
                <Table.Column>思考（是否支持 / 挡位）</Table.Column>
                <Table.Column>速度(tok/s)</Table.Column>
                <Table.Column>首字(s)</Table.Column>
                <Table.Column>成功率</Table.Column>
                <Table.Column>操作</Table.Column>
              </Table.Header>
              <Table.Body>
                {models.map((m) => (
                  <Table.Row key={m.id}>
                    <Table.Cell>
                      <div className="min-w-0">
                        <span className="block font-mono text-[13px] break-all">
                          {m.model_id}
                        </span>
                        {m.display_name && m.display_name !== m.model_id ? (
                          <span className="text-muted block text-[11px]">
                            {m.display_name}
                          </span>
                        ) : null}
                      </div>
                    </Table.Cell>
                    <Table.Cell>{capacityCell(m)}</Table.Cell>
                    <Table.Cell>{priceCell(m)}</Table.Cell>
                    <Table.Cell>{thinkingCell(m)}</Table.Cell>
                    <Table.Cell className="text-xs tabular-nums">
                      {m.tokens_per_sec ? (
                        fmtSpeed(m.tokens_per_sec)
                      ) : (
                        <span className="text-muted">—</span>
                      )}
                    </Table.Cell>
                    <Table.Cell className="text-xs tabular-nums">
                      {m.ttfb_ms ? fmtSec(m.ttfb_ms) : <span className="text-muted">—</span>}
                    </Table.Cell>
                    <Table.Cell className="text-xs tabular-nums">
                      {m.call_count ? (
                        <span className={(m.success_rate ?? 1) < 1 ? 'text-warning' : undefined}>
                          {fmtPercent(m.success_rate ?? 1)}%
                        </span>
                      ) : (
                        <span className="text-muted">—</span>
                      )}
                    </Table.Cell>
                    <Table.Cell>
                      <div className="flex items-center gap-1">
                        {/* 真实发一次 max_tokens=1 的推理请求。与已移除的
                            provider 级测试不同，这个能证明「这个 model_id
                            现在真的能推理」（下架/无权限/名字写错都会红）。 */}
                        <Button
                          variant="outline"
                          size="sm"
                          isDisabled={testing.has(m.id)}
                          aria-label="真实调用一次（max_tokens=1）验证该模型可用"
                          onPress={() => onTest(m)}
                        >
                          {testing.has(m.id) ? '测试中…' : '测试'}
                        </Button>
                        <Button variant="outline" size="sm" onPress={() => onEdit(m)}>
                          编辑
                        </Button>
                        {/* 链首置灰：后端对链首删除回 409，所以这里提前
                             拦住并把「该去哪儿改」写进 aria-label。置灰而不是
                             隐藏：模型确实需要一个删除入口，只是这个模型现在
                             不能删 —— 藏起来会让人以为没有删除功能。 */}
                        <Button
                          variant="danger-soft"
                          size="sm"
                          isDisabled={m.is_route_head}
                          aria-label={
                            m.is_route_head
                              ? '该模型是某条路由的首选目标，不能直接删除 —— 请先到「路由」页改掉那条路由的首选'
                              : `删除模型 ${m.model_id}`
                          }
                          onPress={() => onRemove(m)}
                        >
                          删除
                        </Button>
                      </div>
                    </Table.Cell>
                  </Table.Row>
                ))}
              </Table.Body>
            </Table.Content>
          </Table.ScrollContainer>
        </Table>
      </DataTable>
    </section>
  )
}
