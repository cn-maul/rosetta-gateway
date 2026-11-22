import { Button, Table } from '@heroui/react'
import { Waypoints } from 'lucide-react'
import { Bar, BarChart, CartesianGrid, Cell, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts'
import { DataTable } from '../ui/DataTable'
import { CHART_COLORS, ChartEmpty, ChartTooltipBox, TooltipRow } from '../ui/chart'
import { StatusChip, type StatusTone } from '../ui/StatusChip'
import { fmtSpeed, fmtSec, fmtPercent, fmtTokens } from '../fmt'
import { unitPrice } from '../modelPrice'
import { effortTone } from '../effortTone'
import { thinkingDialectLabel } from '../thinkingDialect'
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
 * 单价按「输入 / 输出 / 缓存」三行竖排。
 *
 * 横排成一行会把价格列撑到 220px 以上，加上操作列的四个按钮后整表必然
 * 横向溢出——最右侧的「成功率 / 操作」被挤出视口，既看不到也点不到。
 * 竖排后每行仍能对齐比较，但列宽不再随单价位数增长。
 * 未配置用「—」而不是「未配置」：完整口径在 title 与编辑弹窗里。
 */
function priceCell(m: UpstreamModel) {
  const parts: { key: string; label: string; value: number }[] = [
    { key: 'in', label: '输入', value: m.price_input },
    { key: 'out', label: '输出', value: m.price_output },
    { key: 'hit', label: '缓存命中', value: m.price_cache_hit },
  ]
  return (
    <div
      className="flex flex-col gap-0.5 text-[11.5px] whitespace-nowrap tabular-nums"
      title={parts.map((p) => `${p.label} ${unitPrice(p.value)}`).join(' · ')}
    >
      {parts.map((p) => (
        <span key={p.key} className="flex items-center gap-1.5">
          <span className="text-muted w-12 shrink-0 whitespace-nowrap">{p.label}</span>
          <span className={p.value > 0 ? 'font-medium' : 'text-muted'}>
            {p.value > 0 ? unitPrice(p.value) : '—'}
          </span>
        </span>
      ))}
    </div>
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
      {/* 方言单独一行：它决定「开关思考」用哪家的字段拼写，和挡位是两个轴。
          只显示生效值（自动时显示推断结果），不显示「配置值」——
          管理员关心的是「这次请求实际按谁的话发」，而不是库里存了什么。 */}
      <span
        className="text-muted text-[11px]"
        title={
          m.thinking_dialect === ''
            ? '自动识别：按模型名推断；认不出来就只发协议标准字段'
            : '手动指定'
        }
      >
        方言 {thinkingDialectLabel(m.effective_thinking_dialect)}
      </span>
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
  onRoute: (m: UpstreamModel) => void
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
  onRoute,
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
            <Table.Content className="min-w-[800px] text-xs [&_td]:px-2 [&_th]:px-2 [&_th]:whitespace-nowrap [&_table]:table-fixed">
              {/* 整表左对齐（用户要求）：数字列也左对齐，与其余运维表一致。
                  表头里把价位顺序写进列名，省得每次都要对着数字猜哪格是哪个。 */}
              <Table.Header>
              <Table.Column isRowHeader className="w-[172px] min-w-[172px] whitespace-nowrap">模型</Table.Column>
              <Table.Column>上下文 / 最大输出</Table.Column>
              <Table.Column className="w-[124px] whitespace-nowrap">单价（元/1M）</Table.Column>
              <Table.Column className="w-[112px] whitespace-nowrap">思考</Table.Column>
              <Table.Column className="w-[76px] whitespace-nowrap">性能</Table.Column>
              <Table.Column className="w-[172px] whitespace-nowrap">操作</Table.Column>
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
                    <Table.Cell>
                      <div
                        className="text-muted flex flex-col gap-0.5 text-[11px] whitespace-nowrap tabular-nums"
                        title={`输出速度 ${m.tokens_per_sec ? `${fmtSpeed(m.tokens_per_sec)} t/s` : '无样本'} · 首字 ${m.ttfb_ms ? `${fmtSec(m.ttfb_ms)}s` : '无样本'} · 成功率 ${m.call_count ? `${fmtPercent(m.success_rate ?? 1)}%` : '无样本'}`}
                      >
                        <span title="输出速度">
                          <b className="text-foreground">{m.tokens_per_sec ? fmtSpeed(m.tokens_per_sec) : '—'}</b> t/s
                        </span>
                        <span title="平均首字">
                          首字 <b className="text-foreground">{m.ttfb_ms ? fmtSec(m.ttfb_ms) : '—'}</b>s
                        </span>
                        <span>
                          成功{' '}
                          <b className={(m.success_rate ?? 1) < 1 ? 'text-warning' : 'text-foreground'}>
                            {m.call_count ? `${fmtPercent(m.success_rate ?? 1)}%` : '—'}
                          </b>
                        </span>
                      </div>
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
                        <Button
                          variant="outline"
                          size="sm"
                          aria-label={`用 ${m.model_id} 创建路由`}
                          onPress={() => onRoute(m)}
                        >
                          <Waypoints size={14} aria-hidden />
                          路由
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

      <ModelSpeedChart models={models} />
    </section>
  )
}

/**
 * 同一上游下各模型的速度对比。
 *
 * 表格已经逐行给了速度、首字与成功率，但「哪个模型明显更快」在一列数字里
 * 读不出来，而这正是换模型/调路由时最直接的依据。只画**有速度样本**的
 * 模型：没调用过的模型没有速度可言，补 0 会把它们画成最慢。
 *
 * 颜色按成功率分档：速度再快、成功率 60% 也不该被当成好选择。
 */
function ModelSpeedChart({ models }: { models: UpstreamModel[] }) {
  const data = models
    .filter((m) => (m.tokens_per_sec ?? 0) > 0)
    .map((m) => ({
      label: m.model_id,
      tps: m.tokens_per_sec ?? 0,
      rate: m.success_rate ?? 1,
      calls: m.call_count ?? 0,
      ttfb: m.ttfb_ms ?? 0,
    }))
    .sort((a, b) => b.tps - a.tps)
    .slice(0, 12)
  if (data.length === 0) {
    return null
  }
  const max = data[0].tps
  return (
    <div className="rounded-2xl border border-separator bg-surface p-4">
      <div className="mb-2 flex flex-wrap items-baseline justify-between gap-2">
        <h3 className="text-sm font-semibold">模型速度</h3>
        <span className="text-muted text-[11.5px]">
          近 5 次成功调用的平均输出速度；颜色表示近 100 次成功率
        </span>
      </div>
      <div className="h-[220px]" role="img" aria-label={`${data.length} 个模型的速度对比`}>
        <ResponsiveContainer width="100%" height="100%">
          <BarChart data={data} layout="vertical" margin={{ top: 2, right: 40, bottom: 2, left: 4 }}>
            <CartesianGrid stroke={CHART_COLORS.grid} strokeDasharray="3 5" horizontal={false} />
            <XAxis
              type="number"
              domain={[0, max * 1.1]}
              tick={{ fill: CHART_COLORS.muted, fontSize: 10.5 }}
              axisLine={false}
              tickLine={false}
              tickFormatter={(v: number) => `${fmtSpeed(v)}`}
            />
            <YAxis
              type="category"
              dataKey="label"
              width={150}
              tick={{ fill: CHART_COLORS.foreground, fontSize: 10.5 }}
              axisLine={false}
              tickLine={false}
              tickFormatter={(value: string) => (value.length > 20 ? value.slice(0, 19) + '…' : value)}
            />
            <Tooltip
              cursor={{ fill: CHART_COLORS.accentSoft }}
              content={({ active, payload }) => {
                if (!active || !payload?.length) return null
                const row = payload[0].payload as (typeof data)[number]
                return (
                  <ChartTooltipBox title={row.label}>
                    <TooltipRow label="速度" value={`${fmtSpeed(row.tps)} t/s`} />
                    <TooltipRow label="首字" value={row.ttfb > 0 ? `${fmtSec(row.ttfb)}s` : '—'} />
                    <TooltipRow label="成功率" value={row.calls > 0 ? `${fmtPercent(row.rate)}%` : '—'} />
                    <TooltipRow label="样本" value={fmtTokens(row.calls)} />
                  </ChartTooltipBox>
                )
              }}
            />
            <Bar dataKey="tps" radius={[0, 4, 4, 0]} maxBarSize={16}>
              {data.map((row) => (
                <Cell
                  key={row.label}
                  fill={
                    row.calls > 0 && row.rate < 0.9
                      ? CHART_COLORS.danger
                      : CHART_COLORS.info
                  }
                />
              ))}
            </Bar>
          </BarChart>
        </ResponsiveContainer>
      </div>
    </div>
  )
}
