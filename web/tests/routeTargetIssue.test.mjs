// 「这一行还不能上线」的提示文案门禁。
//
// 新建路由时第一行是空白的（provider_id 与 upstream_model_id 都是空串）。
// 旧实现直接按查表落空判，于是空白行立刻显示「上游不存在」—— 用户看到这句
// 会以为上游被删了，而实际上只是还没选。所以这里的判据是**顺序**：
// 空 id 必须先于查表被短路掉，填了 id 却查不到才算真的不存在。
//
// 走源码断言而不是 import 组件：Routes.tsx 会连带拉进 react-router-dom 与
// HeroUI，为一个纯函数把整棵依赖树搬进测试不值当，而且真正要守住的是
// 「判空在前、查表在后」这个顺序，源码正是它的载体。

import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

const SRC = join(dirname(fileURLToPath(import.meta.url)), '..', 'src')

function read(rel) {
  return readFileSync(join(SRC, rel), 'utf8')
}

// 取出 targetIssue 的函数体（从 `function targetIssue(` 到下一个顶层 `}`）。
function targetIssueBody(src) {
  const start = src.indexOf('function targetIssue(')
  assert.notEqual(start, -1, 'Routes.tsx 里找不到 targetIssue')
  const open = src.indexOf('{', src.indexOf(')', start))
  let depth = 0
  for (let i = open; i < src.length; i += 1) {
    if (src[i] === '{') depth += 1
    else if (src[i] === '}') {
      depth -= 1
      if (depth === 0) return src.slice(open, i + 1)
    }
  }
  assert.fail('targetIssue 的函数体没有闭合')
}

test('空白目标行不该报「上游不存在」：判空必须在查表之前', () => {
  const body = targetIssueBody(read('pages/Routes.tsx'))
  const providerGuard = body.search(/if\s*\(\s*!providerId\s*\)\s*return\s*''/)
  const modelGuard = body.search(/if\s*\(\s*!modelId\s*\)\s*return\s*''/)
  const providerLookup = body.indexOf('const p = providerMap[providerId]')
  const missingProvider = body.indexOf("'上游不存在'")

  assert.notEqual(providerGuard, -1, 'targetIssue 缺少 `if (!providerId) return \'\'` 短路')
  assert.notEqual(modelGuard, -1, 'targetIssue 缺少 `if (!modelId) return \'\'` 短路')
  assert.notEqual(providerLookup, -1, 'targetIssue 里找不到 providerMap 查表')
  assert.notEqual(missingProvider, -1, "targetIssue 应该在查表落空时才说「上游不存在」")

  assert.ok(
    providerGuard < providerLookup,
    '判空必须先于查表：否则新建路由的空白行会显示「上游不存在」',
  )
  assert.ok(
    providerGuard < missingProvider,
    '「上游不存在」只在填了 provider_id 却查不到时才该出现',
  )
  assert.ok(
    modelGuard < body.indexOf('const m = mmap[modelId]'),
    '模型判空必须先于 mmap 查表：还没选模型不是「上游模型不存在」',
  )
})

test('列表行与编辑弹窗都不为空白目标行渲染告警条', () => {
  const src = read('pages/Routes.tsx')
  // 两处渲染都写成 `{issue ? (<p className="bg-warning-soft ...">` —— 只要
  // 它的取值来自 targetIssue，空白行拿到空串就不会出告警条。这里钉住
  // 「告警条的取值只来自 targetIssue」，防止有人另起一套判断。
  const uses = [...src.matchAll(/const issue = targetIssue\(/g)]
  assert.ok(uses.length >= 2, `告警文案应统一来自 targetIssue，实际只有 ${uses.length} 处`)
  assert.equal(
    /'上游不存在'/.test(src.replace(/\/\/.*$/gm, '')),
    true,
    '「上游不存在」这条文案应保留给真正的缺失（填了却查不到）',
  )
})
