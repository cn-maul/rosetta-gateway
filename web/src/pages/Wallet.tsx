import { useEffect, useMemo, useRef, useState, useSyncExternalStore } from 'react'
import { useNavigate } from 'react-router-dom'
import {
  Alert,
  Button,
  Input,
  Label,
  ListBox,
  ListBoxItem,
  Select,
  SelectIndicator,
  SelectPopover,
  SelectTrigger,
  SelectValue,
  Spinner,
  Table,
  Tabs,
  TextField,
  toast,
} from '@heroui/react'
import { api, ApiFail, isAdmin, session } from '../api'
import { AppModal } from '../ui/AppModal'
import { DataTable } from '../ui/DataTable'
import { StatusChip } from '../ui/StatusChip'
import { fmtNum, fmtTokens, fmtBalance, fmtRemainder, fmtMoney, fmtTimeMs } from '../fmt'
import { rangeStart } from '../range'
import type { Stats, TopupRecord, UsageByUserEntry, UsageGroupEntry, User } from '../types'

/**
 * 钱包与充值：按身份分两种形态的同一页。
 *
 * 管理员账户不再有余额语义 —— 不建 key、不调 API、被禁止给自己充值。
 * 所以管理员进来这一页看的是「别人」的钱：全站消费汇总 + 充值流水 + 充值入口，
 * 刻意没有消耗明细（明细在「调用历史」页，那里才有过滤与 CSV 导出）。
 * 普通用户看的是「我的钱」：余额、按天消耗、自己的充值记录。
 */
export function Wallet() {
  const navigate = useNavigate()
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
  const [topups, setTopups] = useState<TopupRecord[]>([])
  const [topupTotal, setTopupTotal] = useState(0)
  const [topupLoadErr, setTopupLoadErr] = useState('')
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
  const [topupUserId, setTopupUserId] = useState('')
  const [topupAmount, setTopupAmount] = useState('')
  const [topupRemark, setTopupRemark] = useState('')
  const [topupErr, setTopupErr] = useState('')
  const [topupBusy, setTopupBusy] = useState(false)
  const [topupTargets, setTopupTargets] = useState<User[]>([])

  // 请求序号守卫：load() 会在挂载与刷新并发触发，晚到的旧响应不能覆盖新数据。
  const reqSeq = useRef(0)

  const load = async () => {
    const seq = ++reqSeq.current
    setLoading(true)
    setErr('')
    setTopupLoadErr('')
    setByUserErr('')
    setByDayErr('')
    try {
      // now 只取一次：顶部汇总卡（stats）与按用户的表（by-user）是同一区间的
      // 两种视图，各调一次 Date.now() 会差几毫秒，造成自己造出来的「对不上」。
      const now = Date.now()
      const dayFrom = rangeStart(USAGE_DAYS, now)
      const [s, d, t, bu] = await Promise.all([
        // stats 传 (0, now) 即「全部历史」：费用是落库当时固化的 cost_total
        // 再求和，改价不改写历史；且后端合并了日归档，明细被剪掉后不缩水。
        api.stats(0, now),
        // 管理员不请求 by-day（这一格只对普通用户渲染），也不给不用的东西发请求。
        admin
          ? Promise.resolve(null)
          : api
              .usageByDay(dayFrom, now)
              .then((r) => ({ ok: true as const, r }), (e: unknown) => ({ ok: false as const, e })),
        api
          .topups(TOPUP_LIMIT, 0)
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
      // 各区块单独降级：充值流水/按天表挂了不该让余额整块消失。
      if (d === null) {
        setByDay([])
      } else if (d.ok) {
        setByDay(d.r)
      } else {
        setByDay([])
        setByDayErr(d.e instanceof Error ? '消耗记录加载失败：' + d.e.message : '消耗记录加载失败')
      }
      if (t.ok) {
        setTopups(t.r.records)
        setTopupTotal(t.r.total)
      } else {
        setTopups([])
        setTopupTotal(0)
        setTopupLoadErr(t.e instanceof Error ? '充值记录加载失败：' + t.e.message : '充值记录加载失败')
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

  useEffect(() => {
    void load()
  }, [])

  // 余额三字段读 stats 而不是 session.me：me 是登录那一刻的快照，开着页面
  // 余额早已花完而界面不变。me 仅作 stats 未返回瞬间的回退（避免闪「0.00 元」）。
  const unlimited = stats?.balance_unlimited ?? me.me?.balance_unlimited ?? false
  const balanceCents = stats?.balance_cents ?? me.me?.balance_cents ?? 0
  const remainderText = fmtRemainder(stats?.balance_remainder ?? me.me?.balance_remainder ?? 0)
  const balanceEmpty = !unlimited && balanceCents === 0

  // 用户级 token 配额。0 = 不限，必须显式排除，否则未配额度的用户会看到红徽章。
  const quotaTokens = stats?.quota_tokens ?? me.me?.quota_tokens ?? 0
  const usedTokens = stats?.used_tokens ?? me.me?.used_tokens ?? 0
  const quotaExhausted = quotaTokens > 0 && usedTokens >= quotaTokens

  // 管理员形态下这个数字的含义是「所有用户的消费合计」（stats 对管理员返回全局）。
  const totalCost = stats?.cost ?? 0
  const totalCalls = stats?.total_requests ?? 0
  // 0 次调用时不能算均次 —— 会得到除零。
  const avgCost = totalCalls > 0 ? totalCost / totalCalls : 0

  /** 一行的展示名：用户已删除（key 非空而 user_name 空）回退显示 user_id；
   * 无归属（key 为空）显示「无归属」。两者数据形状相同但含义不同。 */
  const rowName = (r: UsageByUserEntry) => (r.user_name ? r.user_name : r.key || '无归属')

  /** 行标记：已删除 / 管理员历史用量 / 无归属。都显式标注，不靠名字空缺意会。 */
  const rowTag = (r: UsageByUserEntry): { text: string; tone: 'dim' | 'info' } | null => {
    if (!r.key) return { text: '无归属', tone: 'dim' }
    if (!r.user_name) return { text: '已删除', tone: 'dim' }
    if (r.role === 'admin') return { text: '管理员·历史', tone: 'info' }
    return null
  }

  const byUserTruncated = byUserTotal > byUser.length
  // 未截断时本表合计必须与顶部「用户消费金额」相等（同一区间、同一固化口径）。
  const byUserSum = byUser.reduce((a, r) => a + r.cost, 0)

  // ---------- 充值 ----------

  /** 充值候选只列普通用户：后端拒绝给管理员充值，列出来是界面自己造的死路。 */
  const topupCandidates = topupTargets.filter((u) => u.role !== 'admin')
  const topupTarget = topupCandidates.find((u) => u.id === topupUserId) ?? null

  /** 元 → 整数分，四舍五入（与后端 store.YuanToCents 同口径）。
   * 刻意在界面侧定死换算：浮点元直接发给后端，对账会差一分钱。 */
  const yuanToCents = (input: string): number | null => {
    const n = Number(input)
    if (!Number.isFinite(n)) return null
    return Math.round(n * 100)
  }

  const openTopup = async () => {
    setTopupErr('')
    setTopupAmount('')
    setTopupRemark('')
    setTopupUserId('')
    setTopupOpen(true)
    try {
      // 用户清单在打开弹窗时才拉：它只服务于这一个弹窗，无条件拉等于每次
      // 进钱包页都多一次 /users（那一页不便宜）。
      setTopupTargets(await api.users())
    } catch (e) {
      if (e instanceof ApiFail && e.status === 401) return
      setTopupErr(e instanceof ApiFail ? e.message : '加载用户列表失败')
    }
  }

  const submitTopup = async () => {
    if (topupBusy) return
    setTopupErr('')
    if (!topupUserId) {
      setTopupErr('请选择要充值的用户')
      return
    }
    const cents = yuanToCents(topupAmount)
    if (cents === null || Number.isNaN(cents)) {
      setTopupErr('请输入一个金额')
      return
    }
    if (cents === 0) {
      // 后端也拒 0，但先说清：空输入框提交上来就是 0 分，而「调平」几乎一定是手滑。
      setTopupErr('金额不能为 0')
      return
    }
    setTopupBusy(true)
    try {
      const updated = await api.adjustUserBalance(topupUserId, cents)
      const verb = cents > 0 ? '充值' : '扣减'
      toast.success(`${verb}成功，${updated.username} 当前余额 ${fmtBalance(updated.balance_cents, updated.balance_unlimited)}`)
      setTopupOpen(false)
      await load()
    } catch (e) {
      setTopupErr(e instanceof ApiFail ? e.message : '操作失败')
    } finally {
      setTopupBusy(false)
    }
  }

  const gotoHistory = () => {
    void navigate('/history')
  }

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
          <Button variant="primary" size="sm" onPress={() => void openTopup()}>
            充值
          </Button>
        ) : null}
        <Button variant="ghost" size="sm" isDisabled={loading} onPress={() => void load()}>
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
            <Tabs.List>
              {tabs.map((t) => (
                <Tabs.Tab key={t.key} id={t.key}>
                  {t.label}
                </Tabs.Tab>
              ))}
            </Tabs.List>
            <Tabs.Panel id={tab}>
              {tab === 'usage' ? (
                admin ? (
                  /* ---------- 管理员：消费汇总（一行一个用户） ---------- */
                  <div>
                    <div className="text-muted mb-3.5 text-[12.5px]">消费汇总 · 全部历史 · 一行一个用户</div>
                    {loading ? (
                      <Spinner role="status">加载中…</Spinner>
                    ) : byUserErr ? (
                      // 这张表失败不替换整块：顶部汇总卡可能已经渲染出来了。
                      <p className="bg-warning-soft text-warning-soft mb-3.5 rounded-md px-2.5 py-2 text-[12.5px]" role="alert">
                        {byUserErr}
                      </p>
                    ) : byUser.length === 0 ? (
                      <div className="py-10 text-center">
                        <div className="text-muted text-2xl">∅</div>
                        <div className="mt-1.5">暂无消费记录</div>
                        <div className="text-muted mx-auto mt-2.5 max-w-[46em] text-[12.5px] leading-relaxed">
                          「全部历史」区间内还没有任何用户的用量记录。
                          <br />
                          若总览页显示有流量，说明这里的加载出了问题而不是真的没有数据 —— 请用上方「刷新」重试。
                        </div>
                      </div>
                    ) : (
                      <>
                        <DataTable label="账户账单" loading={loading} hasData={true} empty={true}>
                          <Table>
                            <Table.ScrollContainer>
                              <Table.Content aria-label="账户账单">
                                <Table.Header>
                                  <Table.Column>用户</Table.Column>
                                  <Table.Column className="text-right">总消费</Table.Column>
                                  <Table.Column className="text-right">调用次数</Table.Column>
                                  <Table.Column className="text-right">Token</Table.Column>
                                </Table.Header>
                                <Table.Body>
                                  {byUser.map((r) => {
                                    const tag = rowTag(r)
                                    return (
                                      <Table.Row key={r.key || '__orphan__'}>
                                        <Table.Cell>
                                          <span className={!r.user_name ? 'font-mono' : ''}>{rowName(r)}</span>
                                          {tag ? (
                                            <span className="ml-1.5">
                                              <StatusChip tone={tag.tone}>{tag.text}</StatusChip>
                                            </span>
                                          ) : null}
                                        </Table.Cell>
                                        {/* 高精度档：两位小数会把小额整列显示成 0.00。 */}
                                        <Table.Cell className="text-right tabular-nums">{r.cost > 0 ? '¥' + fmtMoney(r.cost) : '—'}</Table.Cell>
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
                        {byUserTruncated ? (
                          <p className="bg-warning-soft text-warning-soft mt-3.5 rounded-md px-2.5 py-2 text-[12.5px]" role="alert">
                            共 {fmtNum(byUserTotal)} 位用户有消费，这里只显示消费最高的 {fmtNum(byUser.length)} 位。要看某位用户的逐条明细，请到「调用历史」页按密钥或模型过滤。
                          </p>
                        ) : null}
                        <div className="text-muted mt-3.5 max-w-[46em] text-[12.5px] leading-relaxed">
                          口径：<strong className="text-foreground">全部历史</strong>
                          的按用户汇总，明细已按天归档合并，30 天明细被清理后这些数字也不会缩水。费用是每条用量
                          <strong className="text-foreground">落库当时</strong>按模型单价算好后固化的，改价不会改写历史。
                          <br />
                          本表合计<strong className="text-foreground">¥{fmtMoney(byUserSum)}</strong>
                          {byUserTruncated
                            ? '（仅含上表所示用户，因此小于上方「用户消费金额」）。'
                            : '（与上方「用户消费金额」应当一致；不一致说明有一侧读取失败）。'}
                          全站 {fmtNum(totalCalls)} 次调用、{fmtTokens(stats?.total_tokens ?? 0)} token，失败{' '}
                          {fmtNum(stats?.error_count ?? 0)} 次。
                        </div>
                        <div className="mt-3.5 flex justify-end">
                          <Button variant="ghost" size="sm" onPress={gotoHistory}>
                            查看逐条调用明细 →
                          </Button>
                        </div>
                      </>
                    )}
                  </div>
                ) : (
                  /* ---------- 普通用户：消耗记录（按天一行） ---------- */
                  <div>
                    <div className="text-muted mb-3.5 text-[12.5px]">消耗记录 · 近 {USAGE_DAYS} 天，按天列出</div>
                    {loading ? (
                      <Spinner role="status">加载中…</Spinner>
                    ) : err ? (
                      <p className="bg-warning-soft text-warning-soft mb-3.5 rounded-md px-2.5 py-2 text-[12.5px]" role="alert">
                        {err}
                      </p>
                    ) : byDayErr ? (
                      <p className="bg-warning-soft text-warning-soft mb-3.5 rounded-md px-2.5 py-2 text-[12.5px]" role="alert">
                        {byDayErr}
                      </p>
                    ) : byDay.length === 0 ? (
                      <div className="py-10 text-center">
                        <div className="text-muted text-2xl">∅</div>
                        <div className="mt-1.5">近 {USAGE_DAYS} 天没有消耗记录</div>
                      </div>
                    ) : (
                      <>
                        <DataTable label="账户账单" loading={loading} hasData={true} empty={true}>
                          <Table>
                            <Table.ScrollContainer>
                              <Table.Content aria-label="账户账单">
                                <Table.Header>
                                  <Table.Column>日期</Table.Column>
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
                        <div className="mt-3.5 flex justify-end">
                          <Button variant="ghost" size="sm" onPress={gotoHistory}>
                            查看逐条调用明细 →
                          </Button>
                        </div>
                      </>
                    )}
                    {/* 额度：弱化单行，与余额无关 —— 额度耗尽换 key 即可，
                        余额耗尽必须充值，放一起会被读成同一种东西。 */}
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
              ) : (
                /* ---------- 充值记录：两种身份共用一张表，作用域由服务端决定 ---------- */
                <div>
                  <div className="text-muted mb-3.5 text-[12.5px]">
                    充值记录 · {admin ? `全部用户 · 最近 ${topups.length} 条` : `最近 ${topups.length} 条`}
                  </div>
                  {loading ? (
                    <Spinner role="status">加载中…</Spinner>
                  ) : topupLoadErr ? (
                    <p className="bg-warning-soft text-warning-soft mb-3.5 rounded-md px-2.5 py-2 text-[12.5px]" role="alert">
                      {topupLoadErr}
                    </p>
                  ) : topups.length === 0 ? (
                    <div className="py-10 text-center">
                      <div className="text-muted text-2xl">∅</div>
                      <div className="mt-1.5">暂无充值记录</div>
                      <div className="text-muted mx-auto mt-2.5 max-w-[46em] text-[12.5px]">
                        {admin ? '还没有任何充值操作。' : '还没有人为你的账户充值。'}
                      </div>
                    </div>
                  ) : (
                    <>
                      <DataTable label="账户账单" loading={loading} hasData={true} empty={true}>
                        <Table>
                          <Table.ScrollContainer>
                            <Table.Content aria-label="账户账单">
                              <Table.Header>
                                <Table.Column>时间</Table.Column>
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
                      {topupTotal > topups.length ? (
                        <div className="text-muted mt-3.5 flex justify-end text-[12px]">
                          仅显示最近 {topups.length} / {topupTotal} 条
                        </div>
                      ) : null}
                    </>
                  )}
                </div>
              )}
            </Tabs.Panel>
          </Tabs>
        </>
      )}

      {/* ---------- 充值弹窗（仅管理员） ----------
          「相对调整」的语义与「不限额被切成有限额」必须写在界面上，
          不能靠后端 400 兜底。 */}
      <AppModal
        open={topupOpen}
        onClose={() => setTopupOpen(false)}
        title="充值"
        maxWidth="460px"
        dismissable={false}
        footer={
          <div className="flex justify-end gap-2">
            <Button variant="ghost" onPress={() => setTopupOpen(false)}>
              取消
            </Button>
            <Button variant="primary" isDisabled={topupBusy} onPress={() => void submitTopup()}>
              {topupBusy ? '处理中…' : '确定'}
            </Button>
          </div>
        }
      >
        <div>
          <Label>充值给</Label>
          <div className="mt-1.5">
            <Select
              aria-label="充值给"
              selectedKey={topupUserId === '' ? '__pick__' : topupUserId}
              onSelectionChange={(key) => {
                if (key === null) return
                const v = String(key)
                setTopupUserId(v === '__pick__' ? '' : v)
              }}
            >
              <SelectTrigger>
                <SelectValue />
                <SelectIndicator />
              </SelectTrigger>
              <SelectPopover>
                <ListBox aria-label="充值对象">
                  <ListBoxItem key="pick" id="__pick__">
                    请选择用户
                  </ListBoxItem>
                  {topupCandidates.map((u) => (
                    <ListBoxItem key={u.id} id={u.id}>
                      {u.username} — 当前 {fmtBalance(u.balance_cents, u.balance_unlimited)}
                    </ListBoxItem>
                  ))}
                </ListBox>
              </SelectPopover>
            </Select>
          </div>
        </div>
        {topupCandidates.length === 0 ? (
          <div className="text-warning mt-2 text-[11.5px]">
            还没有可充值的普通用户。管理员账户不参与余额计费，也不能给自己充值。
          </div>
        ) : null}
        <TextField aria-label="金额（元）" value={topupAmount} onChange={setTopupAmount}>
          <Label>金额（元）</Label>
          <Input type="number" step="0.01" placeholder="例如 100" />
          <span className="text-muted text-[11px]">
            填<strong>正数</strong>充值，填<strong>负数</strong>扣减。这是相对调整：填 100 是「加 100 元」，不是「把余额设成 100 元」—— 误操作不会清零。
          </span>
        </TextField>
        <TextField aria-label="备注（可选）" value={topupRemark} onChange={setTopupRemark}>
          <Label>备注（可选）</Label>
          <Input placeholder="充值原因、工单号…" autoComplete="off" />
        </TextField>
        {topupTarget?.balance_unlimited ? (
          <div className="text-warning mt-2 text-[11.5px]">
            {topupTarget.username} 当前<b>不限额</b>。充值会把它切换成有限额 —— 充值后他只能使用填入的金额。
          </div>
        ) : null}
        {topupTarget && (yuanToCents(topupAmount) ?? 0) !== 0 ? (
          <div className="text-muted mt-2 text-[11.5px]">
            调整后约为 <b className="text-foreground">{fmtBalance(topupTarget.balance_cents + (yuanToCents(topupAmount) ?? 0), false)}</b>
          </div>
        ) : null}
        {topupErr ? (
          <div className="text-danger mt-2 text-[12.5px]" role="alert">
            {topupErr}
          </div>
        ) : null}
      </AppModal>
    </div>
  )
}
