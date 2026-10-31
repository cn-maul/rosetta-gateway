import { useEffect, useRef, useState, useSyncExternalStore } from 'react'
import { Alert, Button, Spinner, Tabs, toast } from '@heroui/react'
import { api, ApiFail, isAdmin, session } from '../api'
import { ByDayTable, ByUserTable, TopupTable } from '../components/wallet/WalletTables'
import { TopupModal } from '../components/wallet/TopupModal'
import { fmtNum, fmtTokens, fmtBalance, fmtRemainder, fmtMoney } from '../fmt'
import { rangeStart } from '../range'
import type { Stats, TopupRecord, UsageByUserEntry, UsageGroupEntry } from '../types'

/**
 * 钱包与充值：按身份分两种形态的同一页。
 *
 * 管理员账户不再有余额语义 —— 不建 key、不调 API、被禁止给自己充值。
 * 所以管理员进来这一页看的是「别人」的钱：全站消费汇总 + 充值流水 + 充值入口，
 * 刻意没有消耗明细（明细在「调用历史」页，那里才有过滤与 CSV 导出）。
 * 普通用户看的是「我的钱」：余额、按天消耗、自己的充值记录。
 */
export function Wallet() {
  const me = useSyncExternalStore(session.subscribe, session.get)
  const admin = isAdmin()

  // ---------- 加载状态 ----------
  // 「没拿到」与「拿到但是空的」是两件事：判据用 stats 是否为 null。
  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')
  const [stats, setStats] = useState<Stats | null>(null)
  // 普通用户的消耗记录：按天一行。与总览页日趋势同一个端点，口径天然一致。
  const [byDay, setByDay] = useState<UsageGroupEntry[]>([])
  const [byDayErr, setByDayErr] = useState('')
  // 充值流水。两种身份都要，内容不同由服务端作用域决定。
  // 懒加载：充值页签第一次进入才拉，默认页签（消耗）不等它。
  const [topups, setTopups] = useState<TopupRecord[]>([])
  const [topupTotal, setTopupTotal] = useState(0)
  const [topupLoadErr, setTopupLoadErr] = useState('')
  const [topupsLoading, setTopupsLoading] = useState(true)
  const topupsLoadedRef = useRef(false)
  // 按用户汇总的消费（仅管理员）。
  const [byUser, setByUser] = useState<UsageByUserEntry[]>([])
  const [byUserTotal, setByUserTotal] = useState(0)
  const [byUserErr, setByUserErr] = useState('')

  // 消费汇总一次拉 100（后端 clampLimit 默认值）：这张表是给人读的，
  // 超出不静默截断，表尾会显式说明。
  const BY_USER_LIMIT = 100
  // 按天消耗窗口 90 天：按天一行后 15 天太少，「30 天以前的钱不出现」会让
  // 用户对不上账时以为没扣钱。rangeStart 走本地自然日 0 点，与后端
  // dayExpr（localtime）同口径。
  const USAGE_DAYS = 90
  // 充值流水取 50（后端 ListTopups 默认值）：低频事件，不做分页控件。
  const TOPUP_LIMIT = 50

  // 页签：普通用户「消耗记录」，管理员「消费汇总」。管理员刻意没有明细格。
  const [tab, setTab] = useState<'usage' | 'topup'>('usage')

  // ---------- 管理员充值弹窗 ----------
  const [topupOpen, setTopupOpen] = useState(false)

  // 请求序号守卫：load() 会在挂载与刷新并发触发，晚到的旧响应不能覆盖新数据。
  const reqSeq = useRef(0)
  const topupSeq = useRef(0)

  const load = async () => {
    const seq = ++reqSeq.current
    setLoading(true)
    setErr('')
    setByUserErr('')
    setByDayErr('')
    try {
      // now 只取一次：顶部汇总卡（stats）与按用户的表（by-user）是同一区间的
      // 两种视图，各调一次 Date.now() 会差几毫秒，造成自己造出来的「对不上」。
      const now = Date.now()
      const dayFrom = rangeStart(USAGE_DAYS, now)
      const [s, d, bu] = await Promise.all([
        // stats 传 (0, now) 即「全部历史」：费用是落库当时固化的 cost_total
        // 再求和，改价不改写历史；且后端合并了日归档，明细被剪掉后不缩水。
        api.stats(0, now),
        // 管理员不请求 by-day（这一格只对普通用户渲染），也不给不用的东西发请求。
        admin
          ? Promise.resolve(null)
          : api
              .usageByDay(dayFrom, now)
              .then((r) => ({ ok: true as const, r }), (e: unknown) => ({ ok: false as const, e })),
        // 仅管理员：普通用户打这个端点必然 403，不能无条件发。
        admin
          ? api
              .usageByUser(0, now, BY_USER_LIMIT)
              .then((r) => ({ ok: true as const, r }), (e: unknown) => ({ ok: false as const, e }))
          : Promise.resolve(null),
      ])
      if (seq !== reqSeq.current) return
      setStats(s)
      // 各区块单独降级：按天表挂了不该让余额整块消失。
      if (d === null) {
        setByDay([])
      } else if (d.ok) {
        setByDay(d.r.records)
      } else {
        setByDay([])
        setByDayErr(d.e instanceof Error ? '消耗记录加载失败：' + d.e.message : '消耗记录加载失败')
      }
      if (bu === null) {
        setByUser([])
        setByUserTotal(0)
      } else if (bu.ok) {
        setByUser(bu.r.records)
        setByUserTotal(bu.r.total)
      } else {
        setByUser([])
        setByUserTotal(0)
        setByUserErr(bu.e instanceof Error ? '消费汇总加载失败：' + bu.e.message : '消费汇总加载失败')
      }
    } catch (e) {
      if (seq !== reqSeq.current) return
      // 401 已由 api 层清令牌，AppRoot 的 watch 会跳登录页，这里不抢戏。
      if (e instanceof ApiFail && e.status === 401) return
      // 失败必须留下可见错误态：余额读不出来会被读成「余额是 0」，
      // 而那恰恰会触发 402，让用户去充值一个没发生的故障。
      const msg = '加载钱包数据失败：' + (e instanceof Error ? e.message : String(e))
      setErr(msg)
      toast.danger(msg)
    } finally {
      if (seq === reqSeq.current) setLoading(false)
    }
  }

  const loadTopups = async () => {
    const seq = ++topupSeq.current
    setTopupsLoading(true)
    setTopupLoadErr('')
    try {
      const r = await api.topups(TOPUP_LIMIT, 0)
      if (seq !== topupSeq.current) return
      topupsLoadedRef.current = true
      setTopups(r.records)
      setTopupTotal(r.total)
    } catch (e) {
      if (seq !== topupSeq.current) return
      // 失败也记「拉过」：重试走页内刷新/页头刷新，不能被页签 effect 反复重拉。
      topupsLoadedRef.current = true
      setTopups([])
      setTopupTotal(0)
      setTopupLoadErr(e instanceof Error ? '充值记录加载失败：' + e.message : '充值记录加载失败')
    } finally {
      if (seq === topupSeq.current) setTopupsLoading(false)
    }
  }

  useEffect(() => {
    void load()
  }, [])

  // 充值页签懒加载：第一次进入才拉流水。
  useEffect(() => {
    if (tab === 'topup' && !topupsLoadedRef.current) void loadTopups()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tab])

  // 充值成功：余额与汇总立刻重算；流水表只有用户已经看过才刷（没进过
  // 页签就保持「进入时再拉」，反正拉回来必然是最新数据）。
  const handleTopupSaved = async () => {
    await load()
    if (topupsLoadedRef.current) await loadTopups()
  }

  // 余额三字段读 stats 而不是 session.me：me 是登录那一刻的快照，开着页面
  // 余额早已花完而界面不变。me 仅作 stats 未返回瞬间的回退（避免闪「0.00 元」）。
  const unlimited = stats?.balance_unlimited ?? me.me?.balance_unlimited ?? false
  const balanceCents = stats?.balance_cents ?? me.me?.balance_cents ?? 0
  const remainderText = fmtRemainder(stats?.balance_remainder ?? me.me?.balance_remainder ?? 0)
  const balanceEmpty = !unlimited && balanceCents === 0

  // 用户级 token 配额。0 = 不限，必须显式排除，否则未配额度的用户会看到红徽章。
  const quotaTokens = stats?.quota_tokens ?? me.me?.quota_tokens ?? 0
  const usedTokens = stats?.used_tokens ?? me.me?.used_tokens ?? 0

  // 管理员形态下这个数字的含义是「所有用户的消费合计」（stats 对管理员返回全局）。
  const totalCost = stats?.cost ?? 0
  const totalCalls = stats?.total_requests ?? 0
  // 0 次调用时不能算均次 —— 会得到除零。
  const avgCost = totalCalls > 0 ? totalCost / totalCalls : 0

  const tabs = admin
    ? [
        { key: 'usage', label: '消费汇总' },
        { key: 'topup', label: '充值记录' },
      ]
    : [
        { key: 'usage', label: '消耗记录' },
        { key: 'topup', label: '充值记录' },
      ]

  return (
    <div>
      {/* 充值按钮只对管理员出现。真正的拦截在后端，这里只是不给管理员
          看一个必然失败的入口。 */}
      <div className="mb-3.5 flex items-center justify-end gap-2">
        {admin ? (
          <Button variant="primary" size="sm" onPress={() => setTopupOpen(true)}>
            充值
          </Button>
        ) : null}
        <Button
          variant="ghost"
          size="sm"
          isDisabled={loading}
          onPress={() => {
            void load()
            // 流水表只在用户已经看过时随刷新一起重拉。
            if (topupsLoadedRef.current) void loadTopups()
          }}
        >
          刷新
        </Button>
      </div>

      {/* 骨架必须先于余额出现，否则点进来的第一眼是「空」，与「没钱」无法区分。 */}
      {loading && !stats ? (
        <Spinner role="status">加载中…</Spinner>
      ) : err && !stats ? (
        // 拿不到余额时不渲染任何金额：错误态下渲染「0.00 元」会被读成
        // 「我被清零了」，而那是一次没发生过的账目事故。
        <Alert status="danger">
          <Alert.Content>
            <Alert.Title>{err}</Alert.Title>
            <Alert.Description>余额未能在服务端读取，下面不会显示任何金额。</Alert.Description>
          </Alert.Content>
        </Alert>
      ) : (
        <>
          {/* ---------- 顶部汇总卡：按身份两套，整块换掉而不是条件显示列 ---------- */}
          {admin ? (
            <div className="bg-default-soft mb-3.5 grid grid-cols-1 gap-5 rounded-xl p-6 md:grid-cols-[3fr_1fr_1fr]">
              <div>
                <div className="text-muted text-[12.5px]">用户消费金额</div>
                <div className="my-1.5 text-[40px] leading-[1.15] font-semibold tabular-nums">
                  ¥{fmtMoney(totalCost)}
                </div>
                <div className="text-muted max-w-[34em] text-[12px] leading-relaxed">
                  全部历史累计，共 {fmtNum(totalCalls)} 次调用
                  {totalCalls > 0 ? `，均次 ¥${fmtMoney(avgCost)}` : ''}。
                </div>
              </div>
              <div>
                <div className="text-muted text-[12.5px]">Token 消耗</div>
                <div className="my-1.5 text-[22px] leading-tight font-semibold tabular-nums">
                  {fmtTokens(stats?.total_tokens ?? 0)}
                </div>
                <div className="text-muted max-w-[34em] text-[12px] leading-relaxed">
                  与费用同一区间、同一数据源（输入 + 输出 + 缓存命中）。
                </div>
              </div>
              <div>
                <div className="text-muted text-[12.5px]">调用次数</div>
                <div className="my-1.5 text-[22px] leading-tight font-semibold tabular-nums">{fmtNum(totalCalls)}</div>
                <div className="text-muted max-w-[34em] text-[12px] leading-relaxed">与上方金额同源；单看金额无法判断量级。</div>
              </div>
            </div>
          ) : (
            <div className="bg-default-soft mb-3.5 grid grid-cols-1 gap-5 rounded-xl p-6 md:grid-cols-[2fr_1fr_1fr]">
              <div>
                <div className="text-muted text-[12.5px]">当前余额</div>
                {/* 显示口径必须走 fmtBalance：unlimited 时它返回「不限」。
                    自己写 cents/100 会把不限额显示成「0.00 元」，
                    与「余额耗尽」同形而处置动作相反。 */}
                <div className={`my-1.5 text-[40px] leading-[1.15] font-semibold tabular-nums${balanceEmpty ? ' text-danger' : ''}`}>
                  {unlimited ? '' : '¥'}
                  {fmtBalance(balanceCents, unlimited)}
                </div>
                <div className="text-muted max-w-[34em] text-[12px] leading-relaxed">
                  {unlimited ? '该账户不受余额限制' : '可用余额。耗尽后请求会被拒绝（402），需联系管理员充值。'}
                </div>
                {/* 危险色只给「余额已用尽」：本页唯一对应后端真实 402 的状态。 */}
                {balanceEmpty ? (
                  <div className="text-danger mt-1 text-[12px] font-medium">
                    余额已用尽，现在发起请求会直接失败（HTTP 402），请先联系管理员充值。
                  </div>
                ) : null}
              </div>
              <div>
                <div className="text-muted text-[12.5px]">待结算</div>
                <div className="my-1.5 text-[22px] leading-tight font-semibold tabular-nums">
                  {remainderText ? '¥' + remainderText : '—'}
                </div>
                <div className="text-muted max-w-[34em] text-[12px] leading-relaxed">
                  {remainderText
                    ? '已消费但不足一分、尚未从余额扣除的部分，攒够一分后自动扣除。'
                    : '暂无不足一分的待结算消费。'}
                </div>
              </div>
              <div>
                <div className="text-muted text-[12.5px]">总消费</div>
                <div className="my-1.5 text-[22px] leading-tight font-semibold tabular-nums">¥{fmtMoney(totalCost)}</div>
                <div className="text-muted max-w-[34em] text-[12px] leading-relaxed">
                  全部历史累计，共 {fmtNum(totalCalls)} 次调用
                  {totalCalls > 0 ? `，均次 ¥${fmtMoney(avgCost)}` : ''}。
                </div>
              </div>
            </div>
          )}

          <Tabs
            aria-label="钱包分区"
            selectedKey={tab}
            onSelectionChange={(key) => {
              if (key !== null) setTab(String(key) as 'usage' | 'topup')
            }}
          >
            <Tabs.ListContainer>
              <Tabs.List>
                {tabs.map((t) => (
                  <Tabs.Tab key={t.key} id={t.key}>
                    <Tabs.Indicator />
                    {t.label}
                  </Tabs.Tab>
                ))}
              </Tabs.List>
            </Tabs.ListContainer>
            <Tabs.Panel id={tab}>
              {tab === 'usage' ? (
                admin ? (
                  <ByUserTable
                    byUser={byUser}
                    byUserTotal={byUserTotal}
                    error={byUserErr}
                    loading={loading}
                    totalCalls={totalCalls}
                    errorCount={stats?.error_count ?? 0}
                    totalTokens={stats?.total_tokens ?? 0}
                  />
                ) : (
                  <ByDayTable
                    byDay={byDay}
                    error={byDayErr}
                    loadErr={err}
                    loading={loading}
                    days={USAGE_DAYS}
                    usedTokens={usedTokens}
                    quotaTokens={quotaTokens}
                  />
                )
              ) : (
                <TopupTable
                  topups={topups}
                  total={topupTotal}
                  error={topupLoadErr}
                  loading={topupsLoading}
                  admin={admin}
                />
              )}
            </Tabs.Panel>
          </Tabs>
        </>
      )}

      {/* ---------- 充值弹窗（仅管理员） ----------
          「相对调整」的语义与「不限额被切成有限额」必须写在界面上，
          不能靠后端 400 兜底。 */}
      <TopupModal open={topupOpen} onClose={() => setTopupOpen(false)} onSaved={() => void handleTopupSaved()} />
    </div>
  )
}
