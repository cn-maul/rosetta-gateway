// HeroUI 的 Tabs 是复合组件：底槽（灰底圆角）来自 <Tabs.ListContainer>，
// 选中滑块来自每个 <Tabs.Tab> 里的 <Tabs.Indicator />。
//
// 少了 ListContainer，就只剩 .tabs__list 的 `padding: 4px; display: inline-flex`
// （node_modules/@heroui/styles/dist/heroui.min.css），页签后面没有底色；
// 少了 Indicator，选中项只有 .tabs__tab[data-selected=true] 的文字变色，
// 既没有底色也没有边框 —— 这正是「页内 tab 没有边框、不好看」的成因。
// 两者都是纯装饰 slot，缺失时 React 不报错、类型也过，所以只能靠源码门禁守。
import test from 'node:test'
import assert from 'node:assert/strict'
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join } from 'node:path'

const SRC = new URL('../src/', import.meta.url).pathname.replace(/^\/([A-Za-z]:)/, '$1')

function walk(dir, out = []) {
  for (const name of readdirSync(dir)) {
    const full = join(dir, name)
    if (statSync(full).isDirectory()) walk(full, out)
    else if (full.endsWith('.tsx')) out.push(full)
  }
  return out
}

const files = walk(SRC)
const rel = (f) => f.slice(SRC.length).replace(/\\/g, '/')

test('每个 Tabs 都包了 ListContainer，每个 Tab 都带 Indicator', () => {
  const problems = []
  for (const file of files) {
    const src = readFileSync(file, 'utf8')
    const tabs = src.match(/<Tabs[\s>]/g)?.length ?? 0
    if (!tabs) continue
    const containers = src.match(/<Tabs\.ListContainer[\s>]/g)?.length ?? 0
    const lists = src.match(/<Tabs\.List[\s>]/g)?.length ?? 0
    const indicators = src.match(/<Tabs\.Indicator[\s/>]/g)?.length ?? 0
    // 每个 <Tabs.List> 都必须套在 ListContainer 里
    if (containers !== lists) {
      problems.push(`${rel(file)}: Tabs.List ${lists} 处，ListContainer ${containers} 处`)
    }
    // 每个 <Tabs.Tab> 都必须有一个 Indicator 子元素
    const tabItems = src.match(/<Tabs\.Tab[\s>]/g)?.length ?? 0
    if (indicators < tabItems) {
      problems.push(
        `${rel(file)}: Tabs.Tab ${tabItems} 处，Tabs.Indicator ${indicators} 处 —— 选中态会没有滑块`,
      )
    }
  }
  assert.deepEqual(problems, [], problems.join('\n'))
})

test('设置页默认落在第一个页签（站点），不是模型默认', () => {
  const src = readFileSync(join(SRC, 'pages/Settings.tsx'), 'utf8')
  // 形如：TABS.some((t) => t.key === rawTab) ? (rawTab as TabKey) : TABS[0].key
  const line = src.split('\n').find((l) => l.includes('const tab: TabKey'))
  assert.ok(line, '找不到 Settings 的 tab 推导')
  assert.ok(
    !/:\s*'model'\s*$/.test(line.trim()),
    `设置页默认页签不该写死 'model'：${line.trim()}`,
  )
  assert.ok(
    /:\s*TABS\[0\]\.key\s*$/.test(line.trim()),
    `设置页默认页签应取 TABS[0].key：${line.trim()}`,
  )
  // 首项必须是站点，否则「默认第一个」与用户预期不符。
  const firstKey = src.match(/\{\s*key:\s*'([a-z]+)'\s*,\s*label:/)?.[1]
  assert.equal(firstKey, 'site', `TABS 首项应是站点，实际 ${firstKey}`)
})
