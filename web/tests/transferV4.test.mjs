// 导入导出 v4 的界面契约：新的四段计数必须能显示出来，否则后端搬了数据、
// 用户看到的仍是「新增供应商 N / 新增模型 N」，会以为用户与密钥没导进来。
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'
import assert from 'node:assert/strict'
import test from 'node:test'

const web = join(dirname(fileURLToPath(import.meta.url)), '..')
const read = (rel) => readFileSync(join(web, rel), 'utf8')

test('设置页的导入导出：四段新计数都有回显', () => {
  const src = read('src/pages/Settings.tsx')
  const previewFn = src.match(/function importPreviewRows[\s\S]*?\n\}/)?.[0] ?? ''
  assert.ok(previewFn, '缺少 importPreviewRows')
  for (const field of [
    'user_groups_created',
    'users_created',
    'users_skipped',
    'access_keys_created',
    'access_keys_skipped',
    'routes_created',
    'routes_skipped',
  ]) {
    // 要断「计数源都在面板里」，并确认零值被过滤：只把值塞进数组而不过滤，
    // 结果仍会出现一条「跳过已存在的路由 0」，比不显示更误导。
    assert.ok(previewFn.includes(`p.${field}`), `预演结果缺 ${field} —— 后端导进来了，界面却是哑的`)
  }
  assert.ok(
    previewFn.includes('.filter((row) => row.value > 0)'),
    '预演结果没有过滤零值，会显示一串「新增 0」',
  )
  // 文件内容摘要也要能报出用户/密钥/路由的条数
  const fileFn = src.match(/function importedFileRows[\s\S]*?\n\}/)?.[0] ?? ''
  assert.ok(fileFn, '缺少 importedFileRows')
  assert.ok(fileFn.includes('f.users?.length'), '未解密的文件摘要里没有用户条数')
  assert.ok(fileFn.includes('f.access_keys?.length'), '未解密的文件摘要里没有访问密钥条数')
  assert.ok(fileFn.includes('f.routes?.length'), '未解密的文件摘要里没有路由条数')
})

test('导入导出类型与后端 v4 契约一致', () => {
  const types = read('src/types.ts')
  // 文件体新增四段
  for (const field of ['user_groups?:', 'users?:', 'access_keys?:', 'routes?:']) {
    assert.ok(types.includes(field), `ConfigExportFile 缺 ${field}`)
  }
  // 余额用可选数字：缺省 = 不限额（NULL），与 0 是两种状态
  assert.ok(
    /interface ConfigExportUser \{[\s\S]*balance_cents\?: number/.test(types),
    'ConfigExportUser.balance_cents 必须是可选数字（缺省=不限额）',
  )
  // 密钥必须带明文
  assert.ok(
    /interface ConfigExportAccessKey \{[\s\S]*\n  key: string/.test(types),
    'ConfigExportAccessKey 必须带明文 key',
  )
  // 导入结果新增七段计数
  for (const field of [
    'user_groups_created: number',
    'users_created: number',
    'users_skipped: number',
    'access_keys_created: number',
    'access_keys_skipped: number',
    'routes_created: number',
    'routes_skipped: number',
  ]) {
    assert.ok(types.includes(field), `ConfigImportResult 缺 ${field}`)
  }
})
