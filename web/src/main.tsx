import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { RouterProvider } from 'react-router-dom'
import { Toast } from '@heroui/react'
import { router } from './router'
import './theme.css'

// App 必须能从入口静态可达（router 里懒加载的是页面组件，不影响这点）：
// internal/webui/version_test.go 会从入口 chunk 里找 __APP_VERSION__ 字面量。
createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <Toast.Provider />
    <RouterProvider router={router} />
  </StrictMode>,
)
