import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'

// 上下文窗口 / 最大输出必须走 TokenField（文本输入 + 解析），不能退回
// `<input type="number">`。
//
// 为什么值得一条门禁：数字输入框会把用户敲的 K / M 直接吃掉，输入框看起来
// 像被清空，保存下去是 0（= 未设置，回落默认值）—— 界面上没有任何错误提示，
// 只有模型按错的窗口工作。这种回归最容易在「顺手把字段改回 number」时发生。

const here = dirname(fileURLToPath(import.meta.url))
const webSrc = join(here, '..', 'src')

const read = (rel) => readFileSync(join(webSrc, rel), 'utf8')

/** 数一个字符串里 needle 出现的次数（不用正则，避免转义踩坑）。 */
const count = (haystack, needle) => haystack.split(needle).length - 1

test('token 容量输入统一走 TokenField', () => {
  const modelModal = read('components/provider/ModelEditModal.tsx')
  // 编辑弹窗两个 + 手动添加两个。
  assert.ok(
    count(modelModal, '<TokenField') >= 4,
    '编辑弹窗与手动添加各两处都要用 TokenField（上下文窗口 + 最大输出）',
  )
  // 旧写法：这两个字段曾是 aria-label + Input type="number"。
  for (const old of ['aria-label="上下文窗口"', 'aria-label="最大输出"']) {
    assert.ok(!modelModal.includes(old), `不应再有数字输入框形态的 ${old}`)
  }

  const settings = read('pages/Settings.tsx')
  assert.ok(count(settings, '<TokenField') >= 2, '设置页的默认上下文与默认最大输出都要用 TokenField')
  assert.ok(
    !settings.includes('aria-label="默认上下文窗口（tokens）"'),
    '默认上下文窗口不应退回数字输入框',
  )
  assert.ok(
    !settings.includes('aria-label="默认最大输出（tokens）"'),
    '默认最大输出不应退回数字输入框',
  )
})

test('解析失败必须当场报错，不能静默变 0', () => {
  // `Number(x) || 0` 是这一整类 bug 的源头：解析失败与「填了 0」在这里
  // 长得一模一样，于是错误沉底。两处提交路径都必须先判 null。
  const modelModal = read('components/provider/ModelEditModal.tsx')
  assert.ok(
    modelModal.includes('ctxTokens === null') && modelModal.includes('outTokens === null'),
    '编辑/手动添加都要先判解析结果是否为 null',
  )
  assert.ok(
    !modelModal.includes('context_window: Number('),
    '上下文窗口不允许再用 Number() 直接转换（会用 NaN/丢后缀的方式静默出错）',
  )

  const settings = read('pages/Settings.tsx')
  assert.ok(
    settings.includes('ctxTokens === null') && settings.includes('outTokens === null'),
    '设置页保存前同样要判解析结果',
  )
  assert.ok(
    !settings.includes('default_context_window: Number('),
    '设置页不允许用 Number() 直接转换默认上下文窗口',
  )
})
