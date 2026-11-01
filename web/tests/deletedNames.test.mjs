import test from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'

// 2026-11-01 需求：调用历史里被删除的密钥与上游，不要退化成一串 sha256 id，
// 而是**继续显示当时的名字**，前面加「已删除 · 」。
//
// 这条需求在两个层面都可能悄悄退回去，各自钉一条：
//
//   后端：密钥名原先靠 LEFT JOIN access_keys 现取，删 key 就永久丢名字。
//         现在 usage_records 有 key_name 落库列，SQL 必须优先用它。
//         「对象还在不在」也必须由后端答（deleted 标志）—— 前端没有
//         access_keys / providers 的当前状态，只能靠「名字为空」猜，
//         而空名字既可能是旧记录也可能是被删。
//
//   前端：有 id 没名字时不能再直接把 id 印在格子里。

const read = (rel) => readFile(new URL('../src/' + rel, import.meta.url), 'utf8')

test('后端：密钥名优先取落库固化的 key_name，并用 EXISTS 报 deleted', async () => {
  const go = await readFile(
    new URL('../../internal/admin/usage_handler.go', import.meta.url),
    'utf8',
  )

  assert.ok(
    go.includes("COALESCE(NULLIF(u.key_name, ''), a.name, '')"),
    '调用历史的密钥名没有优先取 usage_records.key_name —— 密钥一删名字就没了',
  )
  assert.ok(
    go.includes("CASE WHEN COALESCE(u.access_key_id, '') <> '' AND a.id IS NULL THEN 1 ELSE 0 END"),
    '缺少 key_deleted 判定：前端无法区分「旧记录没名字」与「密钥被删了」',
  )
  assert.ok(
    go.includes('NOT EXISTS (SELECT 1 FROM providers p WHERE p.id = u.provider_id)'),
    '缺少 provider_deleted 判定',
  )
  assert.ok(go.includes('`json:"key_deleted"`') && go.includes('`json:"provider_deleted"`'),
    '两个 deleted 标志没有下发到 JSON')
})

test('抽屉：已删除的对象带「已删除 · 」前缀', async () => {
  const drawer = await read('components/UsageDetailDrawer.tsx')

  assert.match(
    drawer,
    /export function upstreamName\(h: UsageHistoryEntry\): string \{\s*const name = h\.provider_name \|\| h\.provider_id \|\| '上游未记录'\s*return h\.provider_deleted \? `已删除 · \$\{name\}` : name\s*\}/,
    'upstreamName 没有在 provider_deleted 时加「已删除 · 」前缀',
  )
  assert.match(
    drawer,
    /export function keyName\(h: UsageHistoryEntry\): string \{\s*const name = h\.key_name \|\| h\.key_id \|\| '—'\s*return h\.key_deleted \? `已删除 · \$\{name\}` : name\s*\}/,
    'keyName 没有在 key_deleted 时加「已删除 · 」前缀',
  )
})

test('调用日志：密钥格用 keyName(h)，不再直接印 id', async () => {
  const page = await read('pages/UsageDetail.tsx')

  assert.ok(
    page.includes("{keyName(h)}"),
    '密钥格没有走 keyName(h) —— 删掉的 key 会显示成一串 sha256 id',
  )
  assert.ok(
    !page.includes("{h.key_name || h.key_id || '—'}</span>"),
    '密钥格还在直接拼 key_name/key_id',
  )
  assert.ok(
    page.includes('keyName,') || page.includes('keyName }'),
    'UsageDetail 没有从抽屉模块引进 keyName',
  )
})

test('调用日志：上游已删除时在副行标注，不再说「名称未固化」', async () => {
  const page = await read('pages/UsageDetail.tsx')

  assert.ok(
    page.includes('上游已删除'),
    '上游被删时列表里没有任何标记 —— 名字看起来和活着的上游一样',
  )
  assert.match(
    page,
    /!h\.provider_name && h\.provider_id \? \(\s*<span className="text-\[11px\] text-muted">名称未固化<\/span>\s*\) : h\.provider_deleted \? \(/,
    '「名称未固化」与「上游已删除」没有分成两支：已删除且有名字的行会被标成「名称未固化」',
  )
})
