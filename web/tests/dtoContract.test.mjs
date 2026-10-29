import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'
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
  assert.equal(DTO_CONTRACTS.length, 56)
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

