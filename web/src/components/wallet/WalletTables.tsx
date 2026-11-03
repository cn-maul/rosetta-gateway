import { useNavigate } from 'react-router-dom'
import { Button, Spinner, Table } from '@heroui/react'
import { DataTable } from '../../ui/DataTable'
import { StatusChip } from '../../ui/StatusChip'
import { fmtNum, fmtTokens, fmtMoney, fmtTimeMs } from '../../fmt'
import type { TopupRecord, UsageByUserEntry, UsageGroupEntry } from '../../types'
import { userRowName, userRowTag } from '../../userDisplay'

const softError = (text: string) => (
  <p className="bg-warning-soft text-warning-soft mb-3.5 rounded-md px-2.5 py-2 text-[12.5px]" role="alert">
    {text}
  </p>
)

const emptyBox = (title: string, detail?: string) => (
  <div className="py-10 text-center">
    <div className="text-muted text-2xl">∅</div>
    <div className="mt-1.5">{title}</div>
    {detail ? (
      <div className="text-muted mx-auto mt-2.5 max-w-[46em] text-[12.5px]">{detail}</div>
    ) : null}
  </div>
)

function GotoHistoryLink() {
  const navigate = useNavigate()
  return (
    <div className="mt-3.5 flex justify-end">
      <Button variant="ghost" size="sm" onPress={() => void navigate('/history')}>
        查看逐条调用明细 →
      </Button>
    </div>
  )
}

/** 管理员的消费汇总（一行一个用户）。这张表失败不替换整块：顶部汇总卡
 * 可能已经渲染出来了，所以错误以提示条出现在表格位置。 */
export function ByUserTable({
  byUser,
  byUserTotal,
  error,
  loading,
  totalCalls,
  errorCount,
  totalTokens,
}: {
  byUser: UsageByUserEntry[]
  byUserTotal: number
  error: string
  loading: boolean
  totalCalls: number
  errorCount: number
  totalTokens: number
}) {
  const truncated = byUserTotal > byUser.length
  // 未截断时本表合计必须与顶部「用户消费金额」相等（同一区间、同一固化口径）。
  const sum = byUser.reduce((a, r) => a + r.cost, 0)
  return (
    <div>
      <div className="text-muted mb-3.5 text-[12.5px]">消费汇总 · 全部历史 · 一行一个用户</div>
      {loading ? (
        <Spinner role="status">加载中…</Spinner>
      ) : error ? (
        softError(error)
      ) : byUser.length === 0 ? (
        emptyBox(
          '暂无消费记录',
          '「全部历史」区间内还没有任何用户的用量记录。若总览页显示有流量，说明这里的加载出了问题而不是真的没有数据 —— 请用上方「刷新」重试。',
        )
      ) : (
        <>
          <DataTable label="账户账单" loading={loading} hasData={true}>
            <Table>
              <Table.ScrollContainer>
                <Table.Content aria-label="账户账单">
                  <Table.Header>
                    <Table.Column isRowHeader>用户</Table.Column>
                    <Table.Column className="text-right">总消费</Table.Column>
                    <Table.Column className="text-right">调用次数</Table.Column>
                    <Table.Column className="text-right">Token</Table.Column>
                  </Table.Header>
                  <Table.Body>
                    {byUser.map((r) => {
                      const tag = userRowTag(r)
                      return (
                        <Table.Row key={r.key || '__orphan__'}>
                          <Table.Cell>
                            <span className={!r.user_name ? 'font-mono' : ''}>{userRowName(r)}</span>
                            {tag ? (
                              <span className="ml-1.5">
                                <StatusChip tone={tag.tone}>{tag.text}</StatusChip>
                              </span>
                            ) : null}
                          </Table.Cell>
                          {/* 高精度档：两位小数会把小额整列显示成 0.00。 */}
                          <Table.Cell className="text-right tabular-nums">
                            {r.cost > 0 ? '¥' + fmtMoney(r.cost) : '—'}
                          </Table.Cell>
                          <Table.Cell className="text-right tabular-nums">{fmtNum(r.count)}</Table.Cell>
                          <Table.Cell className="text-right tabular-nums">{fmtTokens(r.tokens)}</Table.Cell>
                        </Table.Row>
                      )
                    })}
                  </Table.Body>
                </Table.Content>
              </Table.ScrollContainer>
            </Table>
          </DataTable>
          {truncated ? (
            <p className="bg-warning-soft text-warning-soft mt-3.5 rounded-md px-2.5 py-2 text-[12.5px]" role="alert">
              共 {fmtNum(byUserTotal)} 位用户有消费，这里只显示消费最高的 {fmtNum(byUser.length)}{' '}
              位。要看某位用户的逐条明细，请到「调用历史」页按密钥或模型过滤。
            </p>
          ) : null}
          <div className="text-muted mt-3.5 max-w-[46em] text-[12.5px] leading-relaxed">
            口径：<strong className="text-foreground">全部历史</strong>
            的按用户汇总，明细已按天归档合并，30 天明细被清理后这些数字也不会缩水。费用是每条用量
            <strong className="text-foreground">落库当时</strong>按模型单价算好后固化的，改价不会改写历史。
            <br />
            本表合计<strong className="text-foreground">¥{fmtMoney(sum)}</strong>
            {truncated
              ? '（仅含上表所示用户，因此小于上方「用户消费金额」）。'
              : '（与上方「用户消费金额」应当一致；不一致说明有一侧读取失败）。'}
            全站 {fmtNum(totalCalls)} 次调用、{fmtTokens(totalTokens)} token，失败 {fmtNum(errorCount)} 次。
          </div>
          <GotoHistoryLink />
        </>
      )}
    </div>
  )
}

/** 普通用户的按天消耗记录。额度刻意保持弱化单行且与余额分开：
 * 额度耗尽换 key 即可，余额耗尽必须充值，放一起会被读成同一种东西。 */
export function ByDayTable({
  byDay,
  error,
  loadErr,
  loading,
  days,
  usedTokens,
  quotaTokens,
}: {
  byDay: UsageGroupEntry[]
  error: string
  loadErr: string
  loading: boolean
  days: number
  usedTokens: number
  quotaTokens: number
}) {
  const quotaExhausted = quotaTokens > 0 && usedTokens >= quotaTokens
  return (
    <div>
      <div className="text-muted mb-3.5 text-[12.5px]">消耗记录 · 近 {days} 天，按天列出</div>
      {loading ? (
        <Spinner role="status">加载中…</Spinner>
      ) : loadErr ? (
        softError(loadErr)
      ) : error ? (
        softError(error)
      ) : byDay.length === 0 ? (
        emptyBox(`近 ${days} 天没有消耗记录`)
      ) : (
        <>
          <DataTable label="账户账单" loading={loading} hasData={true}>
            <Table>
              <Table.ScrollContainer>
                <Table.Content aria-label="账户账单">
                  <Table.Header>
                    <Table.Column isRowHeader>日期</Table.Column>
                    <Table.Column className="text-right">调用次数</Table.Column>
                    <Table.Column className="text-right">Tokens</Table.Column>
                    <Table.Column className="text-right">当日费用</Table.Column>
                  </Table.Header>
                  <Table.Body>
                    {/* 后端 by-day 已按日期升序返回，这里不再排序。 */}
                    {byDay.map((d) => (
                      <Table.Row key={d.key}>
                        <Table.Cell>
                          <span className="font-mono">{d.key}</span>
                        </Table.Cell>
                        <Table.Cell className="text-right tabular-nums">{fmtNum(d.count)}</Table.Cell>
                        <Table.Cell className="text-right tabular-nums">{fmtTokens(d.tokens)}</Table.Cell>
                        {/* cost=0 显示「—」：那通常是未配价，而「未配置」不等于「免费」。 */}
                        <Table.Cell className="text-right tabular-nums">{d.cost > 0 ? '¥' + fmtMoney(d.cost) : '—'}</Table.Cell>
                      </Table.Row>
                    ))}
                  </Table.Body>
                </Table.Content>
              </Table.ScrollContainer>
            </Table>
          </DataTable>
          <GotoHistoryLink />
        </>
      )}
      <div className="border-separator mt-4.5 flex flex-wrap items-baseline gap-2.5 border-t pt-3.5 text-[12.5px]">
        <span className="text-muted">Token 配额</span>
        <span className="tabular-nums">
          已用 {fmtTokens(usedTokens)}
          {quotaTokens > 0 ? ` / ${fmtTokens(quotaTokens)}` : '（不限）'}
        </span>
        <span className="text-muted">额度按 token 计量，耗尽后更换密钥即可；余额按人民币计量，耗尽需充值。</span>
        {quotaExhausted ? <StatusChip tone="err">额度已用尽</StatusChip> : null}
      </div>
    </div>
  )
}

/** 充值记录：两种身份共用一张表，作用域由服务端决定。 */
export function TopupTable({
  topups,
  total,
  error,
  loading,
  admin,
}: {
  topups: TopupRecord[]
  total: number
  error: string
  loading: boolean
  admin: boolean
}) {
  return (
    <div>
      <div className="text-muted mb-3.5 text-[12.5px]">
        充值记录 · {admin ? `全部用户 · 最近 ${topups.length} 条` : `最近 ${topups.length} 条`}
      </div>
      {loading ? (
        <Spinner role="status">加载中…</Spinner>
      ) : error ? (
        softError(error)
      ) : topups.length === 0 ? (
        emptyBox('暂无充值记录', admin ? '还没有任何充值操作。' : '还没有人为你的账户充值。')
      ) : (
        <>
          <DataTable label="账户账单" loading={loading} hasData={true}>
            <Table>
              <Table.ScrollContainer>
                <Table.Content aria-label="账户账单">
                  <Table.Header>
                    <Table.Column isRowHeader>时间</Table.Column>
                    {admin ? <Table.Column>用户</Table.Column> : null}
                    <Table.Column className="text-right">金额</Table.Column>
                    <Table.Column className="text-right">调整后余额</Table.Column>
                    {admin ? <Table.Column>操作者</Table.Column> : null}
                    <Table.Column>备注</Table.Column>
                  </Table.Header>
                  <Table.Body>
                    {topups.map((t) => (
                      <Table.Row key={t.id}>
                        <Table.Cell>
                          <span className="font-mono">{fmtTimeMs(t.ts)}</span>
                        </Table.Cell>
                        {admin ? (
                          <Table.Cell>
                            <span className="font-mono">{t.username || '—'}</span>
                          </Table.Cell>
                        ) : null}
                        {/* 符号是金额列的全部信息量：充值与扣减走同一个端点。 */}
                        <Table.Cell className={`text-right tabular-nums${t.delta_cents < 0 ? ' text-muted' : ''}`}>
                          {t.delta_cents > 0 ? '+' : '−'}¥{fmtMoney(Math.abs(t.delta_cents) / 100)}
                        </Table.Cell>
                        <Table.Cell className="text-right tabular-nums">¥{fmtMoney(t.balance_after / 100)}</Table.Cell>
                        {admin ? (
                          <Table.Cell>
                            <span className="font-mono">{t.operator_username || '—'}</span>
                          </Table.Cell>
                        ) : null}
                        <Table.Cell className="text-muted">{t.remark || '—'}</Table.Cell>
                      </Table.Row>
                    ))}
                  </Table.Body>
                </Table.Content>
              </Table.ScrollContainer>
            </Table>
          </DataTable>
          {/* 截断提示对两种身份都要显示：否则看着 50 行以为那就是全部。 */}
          {total > topups.length ? (
            <div className="text-muted mt-3.5 flex justify-end text-[12px]">
              仅显示最近 {topups.length} / {total} 条
            </div>
          ) : null}
        </>
      )}
    </div>
  )
}
