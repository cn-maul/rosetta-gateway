export interface HeartbeatBucket {
  ts: number
  rate: number | null
  bucket: {
    ts: number
    ok: number
    count: number
    latency: number
  } | null
}

/** Text alternative shared by pointer tooltips and keyboard-readable rows. */
export function heartbeatBucketTexts(
  beats: readonly HeartbeatBucket[],
  formatTs: (ts: number) => string,
): string[] {
  return beats.map((beat) => {
    if (!beat.bucket) return `${formatTs(beat.ts)} · 没有探测记录`
    const pct = Math.round((beat.rate ?? 0) * 100)
    return `${formatTs(beat.bucket.ts)} · 成功率 ${pct}% (${beat.bucket.ok}/${beat.bucket.count}) · 平均 ${beat.bucket.latency}ms`
  })
}
