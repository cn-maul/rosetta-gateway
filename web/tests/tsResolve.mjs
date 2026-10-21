// 让 Node 的 strip-types 能直接 import 前端 TS 源码。
//
// # 为什么需要这个文件
//
// `src/*.ts` 用的是 Vite/TypeScript 的解析规则：相对 import 不带扩展名
// （`from './range'`），裸模块名走 node_modules（`from 'vue'`）。Node 的
// ESM 两者都不接受 —— 前者报 ERR_MODULE_NOT_FOUND，后者真的会去加载
// Vue 运行时（而我们要的是一个可断言的 reactive stub，不是真 Vue）。
//
// 用 `--import` 注册 resolver hook（Node 22 的 module.register）来补齐：
//   - 相对/绝对 import 缺扩展名 → 补 `.ts`；
//   - `vue` → 换成本地 stub。
//
// 这样被测文件 `api.ts` **一行都不用改** —— 改源码来迁就测试是会腐烂的，
// 而测试要盯的恰恰是源码的真实形状。

import { register } from 'node:module'
import { pathToFileURL } from 'node:url'

// 用 register() 而不是 Node 22.15+ 的 registerHooks()：后者跑在**同步**
// 解析阶段，不接受 shortCircuit，而 next() 在内部路径上会返回带该字段的
// 对象 —— 直接 ERR_INVALID_RETURN_PROPERTY_VALUE。register() 是异步
// loader 协议，语义与下面 hooks 的写法匹配（实测可用）。
//
// 代价是 register() 带一条 DEP0205 弃用警告，由 package.json 的 test
// 脚本加 --no-deprecation 消掉：一个每次跑测试都刷屏的警告会训练人
// 忽略警告，而那正是最不该被训练的。
register(new URL('./tsResolveHooks.mjs', import.meta.url), pathToFileURL('./').href)
