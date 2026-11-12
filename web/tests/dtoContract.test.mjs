import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
// 编译器**取自 TS 6 兼容包**，不是项目主依赖 typescript（v7）。
//
// TypeScript 7（Go 原生版）到 7.0 为止**不导出编译器 API**：主入口只剩 version，
// 只有 `typescript/unstable/*` 这套明确标注不稳定的入口。而本文件要做的是读
// `src/types.ts` 的 AST 来核对字段（`createSourceFile` / `isInterfaceDeclaration`
// / `isPropertySignature`），属于典型的 API 消费者。
//
// 所以按微软给出的过渡方案：`tsc` 用 7（typecheck 快 4 倍），这里 import
// `@typescript/typescript6`（`npm:typescript@6.0.2` 的别名）拿 6.0 的 API。
// **7.1 恢复稳定 API 后应把这一行改回 `from 'typescript'` 并删掉兼容包**。
//
// 另一个坑：兼容包自带名为 `tsc` 的 bin，会覆盖 node_modules/.bin/tsc。
// 因此 package.json 的 typecheck 脚本写的是显式路径
// `node ./node_modules/typescript/bin/tsc`，否则会静默跑回 6.0。
import ts from '@typescript/typescript6'
import { DTO_CONTRACTS } from '../src/generated/dto-contract.ts'

const typesPath = fileURLToPath(new URL('../src/types.ts', import.meta.url))
const source = ts.createSourceFile(
  typesPath,
  readFileSync(typesPath, 'utf8'),
  ts.ScriptTarget.Latest,
  true,
  ts.ScriptKind.TS,
)

const interfaces = new Map()
for (const statement of source.statements) {
  if (ts.isInterfaceDeclaration(statement)) {
    interfaces.set(statement.name.text, statement)
  }
}

function propertyNames(declaration, seen = new Set()) {
  if (!declaration || seen.has(declaration.name.text)) return []
  seen.add(declaration.name.text)

  const own = declaration.members
    .filter(ts.isPropertySignature)
    .map((member) => member.name.getText(source))

  const inherited = (declaration.heritageClauses ?? [])
    .flatMap((clause) => clause.types)
    .flatMap((type) => {
      if (!ts.isIdentifier(type.expression)) return []
      return propertyNames(interfaces.get(type.expression.text), seen)
    })

  return [...new Set([...own, ...inherited])]
}

test('Go DTO contract contains every reviewed frontend response type', () => {
  // 60 = 69 - 模型探测两件套（ProbeHealth / ProbeTimelineBucket）
  //      - 模型归组三件套（ModelGroup / ModelGroupHistoryMember / ConfigExportModelGroup）
  //      - 重算用量 / 状态概览 / 代理测试 / 审计条目四个已删除端点。
  //
  // 这个数字是**只增不猜**的守卫：加了一个带 @ts-type 的 Go 结构体就必须
  // 回来改它，从而被迫确认「前端确实有这个接口、字段真的对得上」。
  assert.equal(DTO_CONTRACTS.length, 60)
  const targets = DTO_CONTRACTS.map((contract) => contract.tsType)
  assert.equal(new Set(targets).size, targets.length, '一个 TS 类型不能映射多个 Go DTO')
})

test('web/src/types.ts 与 Go JSON 字段逐项一致', () => {
  const mismatches = []
  for (const contract of DTO_CONTRACTS) {
    const declaration = interfaces.get(contract.tsType)
    if (!declaration) {
      mismatches.push(`types.ts 缺少接口 ${contract.tsType}（Go: ${contract.goType}）`)
      continue
    }

    const actual = propertyNames(declaration).sort()
    const expected = [...contract.fields].sort()
    const missing = expected.filter((field) => !actual.includes(field))
    const extra = actual.filter((field) => !expected.includes(field))
    if (missing.length || extra.length) {
      mismatches.push(
        `${contract.tsType}（${contract.goType}）缺 [${missing.join(', ')}]，多 [${extra.join(', ')}]`,
      )
    }
  }
  assert.deepEqual(mismatches, [])
})
