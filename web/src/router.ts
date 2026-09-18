import { createRouter, createWebHashHistory } from 'vue-router'
import Overview from './views/Overview.vue'
import Providers from './views/Providers.vue'
import Routes from './views/Routes.vue'
import Keys from './views/Keys.vue'

// hash 路由：go:embed 单二进制场景下无需服务端 fallback 配置
const router = createRouter({
  history: createWebHashHistory(),
  routes: [
    { path: '/', name: 'overview', component: Overview, meta: { title: '总览' } },
    { path: '/providers', name: 'providers', component: Providers, meta: { title: '上游与模型' } },
    { path: '/routes', name: 'routes', component: Routes, meta: { title: '路由' } },
    { path: '/keys', name: 'keys', component: Keys, meta: { title: '访问密钥' } },
  ],
})

export default router
