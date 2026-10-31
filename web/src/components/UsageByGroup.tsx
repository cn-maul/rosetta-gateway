import { useCallback, useEffect, useRef, useState } from "react";
import { Button, Table } from "@heroui/react";
import { fmtNum, fmtTokens, fmtMoney, fmtSec, fmtSpeed } from "../fmt";
import { rangeBounds } from "../range";
import { RequestGeneration } from "../asyncState";
import type { UsageGroupEntry, UsageGroupPage } from "../types";
import { DataTable } from "../ui/DataTable";

/**
 * 用量统计的「按维度汇总」面板 —— 上游 / 模型 / 密钥三个页签共用
 * （语义对齐旧 UsageByGroup.vue）。
 *
 * 三个页签的列完全一样，只有第一列的表头与取数端点不同；做成一个组件由
 * props 切换，而不是复制三份 —— 复制后加一列（比如哪天想加「缓存命中」）
 * 就只会改一处，三页的数字对不上，且没人发现。
 */
export interface UsageByGroupProps {
  /** 第一列表头：「上游名称」「模型」「密钥」。 */
  firstCol: string;
  /**
   * 维度。决定副标题的措辞（见 subLabel）—— 不从 firstCol 反推：
   * 表头文案可能被人改，用文案当语义会静默失效。
   */
  kind: "provider" | "model" | "key";
  /**
   * 取数端点。三者都返回 { records, total }（见 UsageGroupPage）——
   * total 是不受 limit 影响的分组数，没有它就分不清「表被截断了」与
   * 「这段时间就只有这么多」。
   */
  fetch: (from: number, to: number) => Promise<UsageGroupPage>;
  /** 时间范围（天），由父页面统一持有。 */
  days: number;
}

/**
 * 成功率。分母用 count（全部请求）而不是 ok+err：canceled 既不是成功也不是
 * 失败，但它确实是一次请求 —— 排除它会让「100% 成功」出现在一个发起了 50 次
 * 调用、其中 40 次被客户端掐断的上游上，那是在夸一个不可用的上游。
 */
function successRate(r: UsageGroupEntry): number {
  return r.count > 0 ? r.ok / r.count : 0;
}

function ratePercent(r: UsageGroupEntry): string {
  return r.count === 0 ? "—" : `${Math.round(successRate(r) * 100)}%`;
}

function rateDetail(r: UsageGroupEntry): string {
  return r.count === 0
    ? "暂无请求"
    : `${fmtNum(r.ok)} 成功 · ${fmtNum(r.err)} 失败`;
}

/** 成功率配色阈值分两档：999 次里失败 3 次比 1 次里失败 1 次严重得多，但百分比一样。 */
function rateTone(r: UsageGroupEntry): "" | "bad" | "mid" {
  const rate = successRate(r);
  if (r.count === 0) return "";
  if (rate < 0.5) return "bad";
  if (rate < 0.9) return "mid";
  return "";
}

/**
 * n_ttfb = 0 表示区间里没有真正的流式请求 —— 显示「—」而不是 0 秒，
 * 后者会被读成「首字快得不需要时间」，真相是「压根没测过」。
 */
function ttfbText(r: UsageGroupEntry): string {
  return r.n_ttfb === 0 ? "—" : `${fmtSec(r.avg_ttfb_ms)}s`;
}

function tpsText(r: UsageGroupEntry): string {
  return r.tps > 0 ? fmtSpeed(r.tps) : "—";
}

/**
 * 首列三级回退：name（显示名）→ key（分组键）→ 占位文案。最后一级不是装饰：
 * 真实库里存在 provider_id 为空串的历史行（无归属的早期记录），name 又带
 * omitempty 会被整个省略 —— 那行既没名字也没键，留白比占位符更像「加载坏了」。
 */
function firstCell(r: UsageGroupEntry): string {
  return r.name || r.key || "未归属请求";
}

/**
 * 副标题：只有显示名与分组键不同时才给，且只有模型页有意义 —— name 是归组
 * 名、key 是上游裸模型名，「归组自 xxx」告诉排查者去哪找这个组。上游页的
 * provider_id 对管理员没有参考价值；真正需要兜底的「name 为空只能显示 key」
 * 那一支已由 firstCell 覆盖，再重复一遍是同一串字符出现两次。
 */
function subLabel(kind: UsageByGroupProps["kind"], r: UsageGroupEntry): string {
  if (!r.name || r.name === r.key) return "";
  return kind === "model" ? `归组自 ${r.key}` : "";
}

/** 记录费用为 0 无法反推出「未配价」还是「免费」，须结合请求日志核对。 */
function costTitle(r: UsageGroupEntry): string {
  return r.cost === 0
    ? "记录费用为 0，可能未配置价格；仅凭汇总无法判断，需结合请求日志核对"
    : "";
}

export function UsageByGroup({
  firstCol,
  kind,
  fetch,
  days,
}: UsageByGroupProps) {
  const [rows, setRows] = useState<UsageGroupEntry[]>([]);
  // total 由后端给，**不能**用 rows.length 顶替：那正好是「把前 N 名当全量」
  // 这个 bug 的来源。null = 还没加载完。
  const [total, setTotal] = useState<number | null>(null);
  const [err, setErr] = useState("");
  const [loading, setLoading] = useState(true);
  const genRef = useRef(new RequestGeneration());

  const load = useCallback(async () => {
    const requests = genRef.current;
    const generation = requests.begin();
    setLoading(true);
    setErr("");
    try {
      // to 现算而不是缓存：页面开着跨过零点时，缓存的 to 会让最新一批数据永远落在窗口外。
      const { from, to } = rangeBounds(days);
      const page = await fetch(from, to);
      if (requests.isCurrent(generation)) {
        setRows(page.records);
        setTotal(page.total);
      }
    } catch (e) {
      if (!requests.isCurrent(generation)) return;
      // 失败必须留可见错误态：空表与「这段时间真的没有调用」长得一模一样，
      // 用户会把一次网关故障读成「没人用过这个上游」。
      setErr("加载用量统计失败：" + (e as Error).message);
      setRows([]);
      setTotal(null);
    } finally {
      if (requests.isCurrent(generation)) setLoading(false);
    }
  }, [days, fetch]);

  useEffect(() => {
    void load();
  }, [load]);

  const unit =
    kind === "provider" ? "个上游" : kind === "model" ? "个模型" : "个密钥";
  // 截断 = 后端说总数比拿到的行多。此时下面那排合计只是前 N 名的合计，
  // 必须当场说清楚，否则它会和总览页的区间合计对不上而没人知道为什么。
  const truncated = total !== null && total > rows.length;
  const totals = rows.reduce(
    (acc, row) => {
      acc.requests += row.count;
      acc.tokens += row.tokens;
      acc.cost += row.cost;
      return acc;
    },
    { requests: 0, tokens: 0, cost: 0 },
  );

  return (
    <DataTable
      label="用量分组统计"
      loading={loading}
      error={err}
      empty={rows.length === 0}
      emptyText="该时段没有调用记录"
      onRetry={load}
      toolbar={
        <div className="flex flex-col gap-2">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <div
              aria-label={
                truncated
                  ? `前 ${rows.length} 名汇总，共 ${total} ${unit}`
                  : "当前范围汇总"
              }
              className="flex flex-wrap items-center gap-2"
            >
              <span className="inline-flex items-center gap-1 rounded-full border border-separator bg-default-soft px-2.5 py-1 text-xs whitespace-nowrap text-muted">
                {truncated ? (
                  <>
                    前{" "}
                    <b className="text-foreground tabular-nums">
                      {fmtNum(rows.length)}
                    </b>{" "}
                    /
                    <b className="text-foreground tabular-nums">
                      {fmtNum(total ?? 0)}
                    </b>{" "}
                    {unit}
                  </>
                ) : (
                  <>
                    <b className="text-foreground tabular-nums">
                      {fmtNum(rows.length)}
                    </b>{" "}
                    {unit}
                  </>
                )}
              </span>
              <span className="inline-flex items-center gap-1 rounded-full border border-separator bg-default-soft px-2.5 py-1 text-xs whitespace-nowrap text-muted">
                <b className="text-foreground tabular-nums">
                  {fmtNum(totals.requests)}
                </b>{" "}
                次请求
              </span>
              <span className="inline-flex items-center gap-1 rounded-full border border-separator bg-default-soft px-2.5 py-1 text-xs whitespace-nowrap text-muted">
                <b className="text-foreground tabular-nums">
                  {fmtTokens(totals.tokens)}
                </b>{" "}
                Tokens
              </span>
              <span className="inline-flex items-center gap-1 rounded-full border border-separator bg-default-soft px-2.5 py-1 text-xs whitespace-nowrap text-muted">
                <b className="text-foreground tabular-nums">
                  ¥{fmtMoney(totals.cost)}
                </b>{" "}
                记录费用
              </span>
            </div>
            <Button
              size="sm"
              variant="secondary"
              isDisabled={loading}
              onPress={load}
            >
              {loading ? "加载中…" : "刷新"}
            </Button>
          </div>
          {truncated ? (
            <div
              className="text-muted rounded-lg border border-separator bg-default-soft px-2.5 py-1.5 text-xs"
              role="note"
            >
              按用量只显示前 {fmtNum(rows.length)} 名，共 {fmtNum(total ?? 0)}{" "}
              {unit}；上面那排合计是这 {fmtNum(rows.length)}{" "}
              名的合计，不是整个区间的合计。
            </div>
          ) : null}
        </div>
      }
    >
      <Table>
        <Table.ScrollContainer>
          <Table.Content aria-label={`用量分组统计，可横向滚动`}>
            <Table.Header>
              <Table.Column isRowHeader>排名</Table.Column>
              <Table.Column>{firstCol}</Table.Column>
              <Table.Column>请求量</Table.Column>
              <Table.Column>Tokens</Table.Column>
              <Table.Column>记录费用</Table.Column>
              <Table.Column>成功率</Table.Column>
              <Table.Column>性能</Table.Column>
            </Table.Header>
            <Table.Body>
              {rows.map((r, i) => {
                const tone = rateTone(r);
                const sub = subLabel(kind, r);
                return (
                  <Table.Row key={r.key}>
                    <Table.Cell className="text-center">
                      <span
                        className={`inline-grid h-6 w-6 place-items-center rounded-lg text-xs font-bold tabular-nums ${
                          i < 3
                            ? "bg-accent-soft text-accent"
                            : "bg-default-soft text-muted"
                        }`}
                      >
                        {i + 1}
                      </span>
                    </Table.Cell>
                    <Table.Cell className="max-w-72">
                      <div className="truncate text-sm font-semibold">
                        {firstCell(r)}
                      </div>
                      {sub ? (
                        <div className="text-muted mt-1 truncate font-mono text-xs">
                          {sub}
                        </div>
                      ) : null}
                    </Table.Cell>
                    <Table.Cell className="text-right tabular-nums">
                      <div className="text-sm font-semibold">
                        {fmtNum(r.count)}
                      </div>
                      <div className="text-muted mt-0.5 text-xs whitespace-nowrap">
                        {rateDetail(r)}
                      </div>
                    </Table.Cell>
                    <Table.Cell className="text-right tabular-nums">
                      <div className="text-sm font-semibold">
                        {fmtTokens(r.tokens)}
                      </div>
                      <div className="text-muted mt-0.5 text-xs whitespace-nowrap">
                        总 token
                      </div>
                    </Table.Cell>
                    <Table.Cell className="text-right tabular-nums">
                      <div title={costTitle(r)}>
                        <div className="text-sm font-semibold">
                          ¥{fmtMoney(r.cost)}
                        </div>
                        <div className="text-muted mt-0.5 text-xs whitespace-nowrap">
                          固化费用
                        </div>
                      </div>
                    </Table.Cell>
                    <Table.Cell className="min-w-40">
                      <div className="flex items-baseline justify-between gap-2.5">
                        <span
                          className={`text-sm font-bold tabular-nums ${
                            tone === "bad"
                              ? "text-danger"
                              : tone === "mid"
                                ? "text-warning"
                                : ""
                          }`}
                        >
                          {ratePercent(r)}
                        </span>
                        <span className="text-muted text-xs whitespace-nowrap">
                          {rateDetail(r)}
                        </span>
                      </div>
                      <div
                        aria-hidden="true"
                        className="mt-1.5 h-1 overflow-hidden rounded-full bg-default-soft"
                      >
                        <span
                          className={`block h-full rounded-full ${
                            tone === "bad"
                              ? "bg-danger"
                              : tone === "mid"
                                ? "bg-warning"
                                : "bg-success"
                          }`}
                          style={{
                            width: `${Math.max(0, Math.min(100, successRate(r) * 100))}%`,
                          }}
                        />
                      </div>
                    </Table.Cell>
                    <Table.Cell>
                      <div className="flex flex-wrap items-center gap-1.5">
                        <span className="inline-flex items-baseline gap-1 rounded-md bg-default-soft px-1.5 py-0.5 text-xs whitespace-nowrap text-muted">
                          首字{" "}
                          <b className="text-foreground tabular-nums">
                            {ttfbText(r)}
                          </b>
                        </span>
                        <span className="inline-flex items-baseline gap-1 rounded-md bg-default-soft px-1.5 py-0.5 text-xs whitespace-nowrap text-muted">
                          速度{" "}
                          <b className="text-foreground tabular-nums">
                            {tpsText(r)} t/s
                          </b>
                        </span>
                      </div>
                    </Table.Cell>
                  </Table.Row>
                );
              })}
            </Table.Body>
          </Table.Content>
        </Table.ScrollContainer>
      </Table>
    </DataTable>
  );
}
