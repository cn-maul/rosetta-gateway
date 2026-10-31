// 「缓存写入价不再暴露」与「上游详情按钮描边」的源码门禁。
//
// 守三件只在界面上才成立、类型系统完全不管的事：
//
//  1) 缓存写入（Anthropic cache_creation）这一档价格不许再出现在任何输入框里。
//     用户 2026-10-31 的要求是「所有界面都去掉」。库里那一列还在（计费要用，
//     未配置时按输入价算），但界面上多一格空位只会让人以为漏填。这条断言按
//     「文件 × 该文件里是否允许出现这个词」逐条写死，比全仓 grep 更准 ——
//     UsageDetail 的 token 口径文案（缓存写入 X tokens）必须留着。
//
//  2) 批量改价绝不能顺手清掉缓存写入价。后端 2026-10-31 把该字段改成指针语义
//     （不带 = 保持原值），所以前端**不许**再往 batch-price 里塞这个字段：塞了
//     就是一次静默的数据丢失，而界面看上去一切正常。
//
//  3) 上游详情页的按钮要看得见。secondary/ghost 在亮色下几乎与卡片同色，用户
//     的原话是「加一个描边」。HeroUI 里带描边的是 variant="outline"（它的 CSS
//     有 border-width:1px + border-color:var(--border)），所以这一页子树里不许
//     再出现 secondary / ghost。

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const WEB = join(dirname(fileURLToPath(import.meta.url)), '..')
const read = (rel) => readFile(join(WEB, rel), 'utf8')

/** 去掉注释，避免注释里提到「缓存写入」把断言带偏。 */
function stripComments(src) {
  return src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|[^:])\/\/[^\n]*/g, '$1')
}

test('价格输入不再暴露「缓存写入」这一档', async () => {
  // 这几个文件负责「填价格」：任何一处留下缓存写入输入框，用户就会看到空格子。
  const priceFiles = [
    'src/components/price/priceShared.tsx',
    'src/components/price/PriceEditModal.tsx',
    'src/components/price/BatchPriceModal.tsx',
    'src/components/PriceSettings.tsx',
    'src/components/provider/ModelEditModal.tsx',
    'src/pages/Models.tsx',
  ]
  for (const f of priceFiles) {
    const code = stripComments(await read(f))
    assert.ok(
      !code.includes('缓存写入'),
      `${f} 仍在界面上提到「缓存写入」—— 用户要求所有界面都去掉这一档价格`,
    )
  }

  // 但 priceShared 里有两处**必须**保留 price_cache_creation：
  //  - PriceFormState / emptyPrices 是表单模型 → 不许有（上面已按文件断言）
  //  - isPriced(m) 判的是库里的模型记录：「只配了缓存写入价」也算配过价，
  //    去掉这一项会让这类模型在列表里被当成没配价。
  // 所以这里精确断言表单模型那一段，而不是整文件 grep。
  const shared = stripComments(await read('src/components/price/priceShared.tsx'))
  const formModel = shared.slice(shared.indexOf('interface PriceFormState'), shared.indexOf('export function PriceField'))
  assert.ok(
    !formModel.includes('price_cache_creation'),
    'PriceFormState / emptyPrices 是三档表单模型，不该再声明 price_cache_creation',
  )
})

test('批量改价不发送 price_cache_creation（否则会静默清空既有值）', async () => {
  const api = stripComments(await read('src/api.ts'))
  const batchSig = api.slice(api.indexOf('batchUpdateModelPrices:'))
  const decl = batchSig.slice(0, batchSig.indexOf('=>'))
  assert.ok(
    !decl.includes('price_cache_creation'),
    'api.batchUpdateModelPrices 的参数类型不许包含 price_cache_creation —— 后端对它是指针语义（不带 = 保持原值）',
  )

  const modal = stripComments(await read('src/components/price/BatchPriceModal.tsx'))
  assert.ok(
    !modal.includes('price_cache_creation'),
    'BatchPriceModal 不许再拼 price_cache_creation 进请求体',
  )
})

test('手动添加模型能填三档单价', async () => {
  const src = stripComments(await read('src/components/provider/ModelEditModal.tsx'))
  // submitManual 里必须把三档价格发出去 —— 只填 model_id 的话，现场添加模型的人
  // 还得回列表逐条补价，白跑一趟。
  const start = src.indexOf('const submitManual')
  assert.ok(start > 0, '找不到 submitManual')
  // 截到下一个「顶层 const」（两空格缩进）为止 —— 中间那些 `const id = ...`
  // 是函数体内部变量，不能当边界。
  const rest = src.slice(start + 10)
  const nextTopLevel = rest.indexOf('\n  const ')
  const body = nextTopLevel < 0 ? rest : rest.slice(0, nextTopLevel)
  for (const field of ['price_input', 'price_cache_hit', 'price_output']) {
    assert.ok(body.includes(field), `submitManual 没有发送 ${field}，手动添加的模型就没有价`)
  }
})

test('上游详情页的按钮都带描边', async () => {
  const files = [
    'src/pages/ProviderDetail.tsx',
    'src/components/provider/ProviderOverviewCard.tsx',
    'src/components/CredentialPanel.tsx',
    'src/components/UpstreamModelPanel.tsx',
  ]
  for (const f of files) {
    const code = stripComments(await read(f))
    assert.ok(
      !/variant="(secondary|ghost|tertiary)"/.test(code),
      `${f} 还有无描边的按钮变体 —— 这一页的按钮统一用 outline`,
    )
  }

  // 返回入口是页级动作，要按钮不要正文里的链接。
  const detail = stripComments(await read('src/pages/ProviderDetail.tsx'))
  assert.ok(
    !/<Link\s+to=\{backTo\}/.test(detail),
    '上游详情的「返回供应商」必须是 Button，不能是 Link',
  )
  assert.ok(
    detail.includes('← 供应商'),
    '返回按钮的文案丢了（← 供应商）',
  )
})
