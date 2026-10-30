import { Button, Table } from '@heroui/react'
import { DataTable } from '../ui/DataTable'
import { fmtSpeed, fmtSec, fmtPercent } from '../fmt'
import { modelPriceLine } from '../modelPrice'
import type { UpstreamModel } from '../types'

/**
 * 上游模型面板：某个上游下声明的模型列表（详情页下半部分）。
 *
 * 与旧 UpstreamModelPanel.vue 相同的取舍：只搬展示，写操作
 * （测试/编辑/删除）由父组件持有 —— 父组件是唯一的写路径。
 */

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
        <Button variant="secondary" size="sm" onPress={onAdd}>
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
            <Table.Content className="min-w-[860px]">
              <Table.Header>
                <Table.Column>模型</Table.Column>
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
                        <span className="block font-mono text-[13px]">{m.model_id}</span>
                        {/* 价格与思考挡位是配置摘要：agent 工具靠 /v1/models 的
                             supported_reasoning 决定给用户开几档，那份数据就来自
                             这里的配置。不显示的话「客户端以为支持、实际被静默
                             降级」无从追查。能否思考 / 有哪几档必须一起显示：
                             只显示挡位会让人以为「配了档位就等于支持思考」，而
                             数据面在「不支持思考」时会直接把思考配置剥掉。 */}
                        <span className="text-muted mt-0.5 block text-xs break-all">
                          价格（元/1M）：{modelPriceLine(m)}
                        </span>
                        {m.supports_thinking !== undefined ? (
                          <span className="text-muted block text-xs">
                            思考：{m.supports_thinking ? '支持' : '不支持'}
                          </span>
                        ) : null}
                        {m.effort_levels?.length ? (
                          <span className="text-muted block text-xs">
                            挡位：{m.effort_levels.join(' · ')}
                            {m.unknown_effort_levels?.length ? (
                              <span className="text-muted">
                                （未识别：{m.unknown_effort_levels.join('、')}）
                              </span>
                            ) : null}
                          </span>
                        ) : null}
                      </div>
                    </Table.Cell>
                    <Table.Cell className="text-right">
                      <span className="text-muted text-xs">
                        {m.tokens_per_sec ? fmtSpeed(m.tokens_per_sec) : '—'}
                      </span>
                    </Table.Cell>
                    <Table.Cell className="text-right">
                      <span className="text-muted text-xs">
                        {m.ttfb_ms ? fmtSec(m.ttfb_ms) : '—'}
                      </span>
                    </Table.Cell>
                    <Table.Cell className="text-right">
                      {m.call_count ? (
                        <span
                          className={
                            (m.success_rate ?? 1) < 1
                              ? 'text-warning text-xs'
                              : 'text-xs'
                          }
                        >
                          {fmtPercent(m.success_rate ?? 1)}%
                        </span>
                      ) : (
                        <span className="text-muted text-xs">—</span>
                      )}
                    </Table.Cell>
                    <Table.Cell>
                      <div className="flex items-center gap-1">
                        {/* 真实发一次 max_tokens=1 的推理请求。与已移除的
                             provider 级测试不同，这个能证明「这个 model_id
                             现在真的能推理」（下架/无权限/名字写错都会红）。 */}
                        <Button
                          variant="ghost"
                          size="sm"
                          isDisabled={testing.has(m.id)}
                          aria-label="真实调用一次（max_tokens=1）验证该模型可用"
                          onPress={() => onTest(m)}
                        >
                          {testing.has(m.id) ? '测试中…' : '测试'}
                        </Button>
                        <Button variant="ghost" size="sm" onPress={() => onEdit(m)}>
                          编辑
                        </Button>
                        {/* 链首置灰：后端对链首删除回 409，所以这里提前
                             拦住并把「该去哪儿改」写进 aria-label。置灰而不是
                             隐藏：模型确实需要一个删除入口，只是这个模型现在
                             不能删 —— 藏起来会让人以为没有删除功能。 */}
                        <Button
                          variant="ghost"
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
