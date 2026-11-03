// 表单可辨识度与「添加上游模型」两档入口的源码门禁。
//
// 守四件在浏览器里才看得见、类型系统完全不管的事：
//
//  1) 输入类控件必须和卡片底色分得开。HeroUI 默认 `--field-background: var(--white)`
//     且 `--field-border: transparent` / `--field-border-width: 0px`，而卡片底也是
//     纯白 —— 输入框只剩 1px 阴影，肉眼分辨不出「哪里能打字」。修法只允许出现在
//     theme.css 的令牌覆盖里（D1：组件里不写魔法值），所以这里断言令牌存在且
//     边框宽度不为 0。
//
//  2) 表单里并排的「显示名称 + 接口方式」必须都带可见标签。Select 只给 aria-label
//     时没有 Label 占位，trigger 会顶到格子顶部，和旁边带 Label 的输入框错开一行。
//
//  3) 探测到的模型列表必须是单列。模型名常带厂商前缀（deepseek/deepseek-chat），
//     两列会把名字截断并挤掉右侧的「思考 / ctx」提示。
//
//  4) 探测拿不到列表时要有「手动添加模型」这一档 —— 有些上游不实现 /models。

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readdir, readFile } from 'node:fs/promises'
import { dirname, join, relative, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

const WEB = join(dirname(fileURLToPath(import.meta.url)), '..')
const read = (rel) => readFile(join(WEB, rel), 'utf8')

/** 递归列出 src 下的 tsx，返回相对 WEB 的路径（统一用 /）。 */
async function listTsx(dir) {
  const out = []
  async function walk(abs) {
    for (const e of await readdir(abs, { withFileTypes: true })) {
      const full = join(abs, e.name)
      if (e.isDirectory()) {
        if (e.name === 'node_modules' || e.name === 'dist') continue
        await walk(full)
      } else if (e.name.endsWith('.tsx')) {
        out.push(relative(WEB, full).split(sep).join('/'))
      }
    }
  }
  await walk(join(WEB, dir))
  return out
}

test('输入控件的底色与边框在 theme.css 里被显式覆盖', async () => {
  const css = await read('src/theme.css')

  // 底色不能和卡片同色：用 surface-secondary 这条已有的语义令牌，而不是写死色值。
  assert.match(
    css,
    /--field-background:\s*var\(--surface-secondary\)/,
    'theme.css 必须把 --field-background 退到 --surface-secondary，否则输入框与卡片同色',
  )
  assert.match(
    css,
    /--field-border:\s*var\(--border\)/,
    'theme.css 必须给 --field-border 指定颜色，默认是 transparent',
  )
  // 只有颜色没有宽度依旧画不出边框：HeroUI base 层把 --field-border-width 定死 0px。
  assert.match(
    css,
    /--field-border-width:\s*1px/,
    'theme.css 必须把 --field-border-width 从 0px 提到 1px，否则设了颜色也没有边框',
  )
})

test('上游表单里并排的「显示名称 / 接口方式」都带可见标签', async () => {
  for (const rel of ['src/components/provider/ProviderEditModal.tsx', 'src/pages/Providers.tsx']) {
    const src = await read(rel)
    assert.ok(
      src.includes('<Label>接口方式</Label>'),
      `${rel}: 接口方式这个 Select 缺少可见 Label（只有 aria-label 时它会和左边的输入框错开一行）`,
    )
    assert.ok(
      !src.includes('aria-label="类型"'),
      `${rel}: 还有只给 aria-label="类型" 的 Select —— 没有 Label 占位会与同排输入框错位`,
    )
  }
})

test('发现到的模型按单列渲染', async () => {
  const src = await read('src/components/provider/ModelEditModal.tsx')
  // 定位到列表容器本身（唯一带 max-h-[320px] 的那个 div）。
  const at = src.indexOf('max-h-[320px]')
  assert.ok(at > 0, '找不到模型列表容器')
  const tag = src.slice(src.lastIndexOf('<div', at), src.indexOf('>', at) + 1)
  assert.ok(
    !tag.includes('sm:grid-cols-2'),
    `模型列表不许再切成两列（模型名长，两列会截断并挤掉右侧提示）：${tag}`,
  )
  assert.ok(tag.includes('grid-cols-1'), `模型列表应显式声明 grid-cols-1：${tag}`)
})

test('探测不到列表时能手动添加模型', async () => {
  const src = await read('src/components/provider/ModelEditModal.tsx')
  assert.ok(src.includes('手动添加模型'), '缺少「手动添加模型」入口')
  assert.ok(src.includes('返回获取列表'), '手动档需要能切回探测档')
  assert.ok(src.includes('submitManual'), '缺少手动添加的提交函数')
  // 手动档也走同一个导入接口：后端 ImportModels 会把 <=0 的窗口/输出回落到默认值，
  // 前端另起一条创建接口只会多一套校验口径。
  assert.match(
    src.slice(src.indexOf('const submitManual')),
    /api\.importModels\(/,
    '手动添加必须复用 api.importModels',
  )
})

// HeroUI 3 的 Checkbox 是复合组件：`Checkbox` 本身只渲染一个空 div，
// 真正的 <input> 来自 Checkbox.Content → Checkbox.Control。
// 只传文本子节点时编译能过、界面上也有字，但既看不见方框也点不动 ——
// 「模型白名单只能全选/清空」正是这样发生的。类型系统管不到，只能源码门禁。
test('每个 Checkbox 都渲染了可点击的 Content/Control', async () => {
  const files = (await listTsx('src'))
  const violations = []
  for (const rel of files) {
    const src = await read(rel)
    // (?![.\w]) 排除 <Checkbox.Content> / <Checkbox.Control> 这类子组件。
    for (const m of src.matchAll(/<Checkbox(?![.\w])[\s\S]*?<\/Checkbox>/g)) {
      if (!m[0].includes('<Checkbox.Content')) violations.push(rel + ': ' + m[0].slice(0, 80))
    }
    // 自闭合的 Checkbox 同样没有 input（例如表格里的裸勾选框）。
    for (const m of src.matchAll(/<Checkbox(?![.\w])[^>]*\/>/g)) violations.push(rel + ': ' + m[0].slice(0, 80))
  }
  assert.deepEqual(
    violations,
    [],
    'Checkbox 缺少 Content/Control，会渲染成空 div、点不动：\n' + violations.join('\n'),
  )
})
