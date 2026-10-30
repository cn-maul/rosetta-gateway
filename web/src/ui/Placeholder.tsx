import { useState } from 'react'
import { Button, Modal, toast } from '@heroui/react'

/**
 * P1 骨架验证页：18 条路由的临时落点。
 *
 * 顺手验证整条技术栈链路 —— HeroUI Button（onPress 语义）、Modal（受控
 * isOpen，焦点陷阱/滚动锁/Esc 由 React Aria 承担）、toast（命令式 API，
 * 根部 Toast.Provider 已在 main.tsx 挂载）、Tailwind 工具类与暗色令牌。
 * P2 建好 src/ui/Shell 与各页面后整体删除。
 */
export function Placeholder({ name }: { name: string }) {
  const [open, setOpen] = useState(false)
  return (
    <main className="flex min-h-dvh flex-col items-center justify-center gap-4 bg-background text-foreground">
      <h1 className="text-2xl font-semibold">{name}</h1>
      <p className="text-muted max-w-app text-center text-sm">
        React 19 + Tailwind v4 + HeroUI v3 骨架已就绪；本页是重建期间的占位，将在后续阶段被真实页面替换。
      </p>
      {/* 版本号必须始终有人渲染：internal/webui/version_test.go 从入口 chunk 找
          __APP_VERSION__ 字面量，P2 的 Shell 页脚接手之前由本页保住这条链路。 */}
      <footer className="text-muted text-xs">
        v{__APP_VERSION__} · rosetta {__ROSETTA_VERSION__}
      </footer>
      <div className="flex gap-2">
        <Button variant="primary" onPress={() => toast.success('toast 链路可用')}>
          toast 测试
        </Button>
        <Button variant="secondary" onPress={() => setOpen(true)}>
          Modal 测试
        </Button>
      </div>
      <Modal isOpen={open} onOpenChange={(v) => !v && setOpen(false)}>
        <Modal.Backdrop>
          <Modal.Container>
            <Modal.Dialog>
              <Modal.Header>
                <Modal.Heading>骨架验证</Modal.Heading>
              </Modal.Header>
              <Modal.Body>焦点陷阱、滚动锁与 Esc 关闭由 React Aria 承担，App.vue 的手写实现已删除。</Modal.Body>
              <Modal.Footer>
                <Button variant="tertiary" onPress={() => setOpen(false)}>
                  关闭
                </Button>
              </Modal.Footer>
            </Modal.Dialog>
          </Modal.Container>
        </Modal.Backdrop>
      </Modal>
    </main>
  )
}
