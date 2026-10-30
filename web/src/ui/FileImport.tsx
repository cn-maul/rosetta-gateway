import { useRef } from 'react'
import { Button } from '@heroui/react'

/**
 * 文件选择（导入配置用）。HeroUI 没有 FileTrigger 之类的等价组件，
 * 系统文件对话框只能由原生 <input type="file"> 触发 —— 把它封装在这一处，
 * 两份门禁（legacy-audit 与 noHandWrittenPrimitives）的白名单都只登记
 * ui/FileImport 这个前缀，其余代码只见到 onPick(File) 回调。
 */
export function FileImport({
  accept,
  label = '选择文件…',
  disabled,
  onPick,
}: {
  accept: string
  label?: string
  disabled?: boolean
  onPick: (file: File) => void
}) {
  const inputRef = useRef<HTMLInputElement>(null)
  return (
    <>
      <Button
        variant="secondary"
        size="sm"
        isDisabled={disabled}
        onPress={() => inputRef.current?.click()}
      >
        {label}
      </Button>
      <input
        ref={inputRef}
        type="file"
        accept={accept}
        className="hidden"
        onChange={(e) => {
          const f = e.target.files?.[0]
          if (f) onPick(f)
          // 允许重选同一个文件：不清 value 就不会再次触发 change。
          e.target.value = ''
        }}
      />
    </>
  )
}
