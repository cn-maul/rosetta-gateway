import { useEffect, useState } from 'react'
import { Button, Input, Label, TextField, toast } from '@heroui/react'
import { api } from '../../api'
import { SectionCard } from '../../ui/SectionCard'
import { BrandMark } from '../../ui/BrandMark'
import { FileImport } from '../../ui/FileImport'
import { setBranding } from '../../ui/branding'
import { DEFAULT_SITE_NAME } from '../../ui/branding'
import type { SiteBranding } from '../../types'

/**
 * 站点设置：站点名称 + LOGO。
 *
 * LOGO 直接存成 data URL 进 app_settings，不落文件系统 —— 这个后台是
 * 「一个可执行文件 + 一个 SQLite」的部署形态，多一个上传目录就多一份
 * 备份/权限要操心的事；256KB 的上限也够放一张干净的标志图。
 *
 * 前端自己先按同一套规则校验一遍（尺寸、格式），但**以后端为准**：
 * 前端校验只是省一次往返，不是安全边界。
 */

/** 与后端 uploadSiteLogo 上限一致（解码后字节数）。 */
const MAX_LOGO_BYTES = 256 * 1024
const MAX_NAME_RUNES = 64

function readFileAsDataURL(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => resolve(String(reader.result ?? ''))
    reader.onerror = () => reject(new Error('读取文件失败'))
    reader.readAsDataURL(file)
  })
}

/** data URL 的 base64 段解码后的字节数；不是合法 data URL 时返回 -1。 */
function dataUrlBytes(dataUrl: string): number {
  const idx = dataUrl.indexOf(';base64,')
  if (idx < 0) return -1
  const b64 = dataUrl.slice(idx + ';base64,'.length)
  try {
    // atob 给出的是「每字符一字节」的二进制串，长度即字节数。
    return atob(b64).length
  } catch {
    return -1
  }
}

export function SiteBrandCard({ onSaved }: { onSaved?: () => void }) {
  const [name, setName] = useState('')
  const [logo, setLogo] = useState('')
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    let alive = true
    void (async () => {
      try {
        const s = await api.site()
        if (!alive) return
        setName(s.site_name ?? '')
        setLogo(s.site_logo ?? '')
      } catch (e) {
        if (!alive) return
        // 读失败不阻断编辑：管理员可以照着现在的样子重填。
        toast.danger('站点设置加载失败：' + (e as Error).message)
      } finally {
        if (alive) setLoading(false)
      }
    })()
    return () => {
      alive = false
    }
  }, [])

  const pickLogo = async (file: File | null) => {
    if (!file) return
    if (!file.type.startsWith('image/')) {
      toast.danger('请选择图片文件（PNG / SVG / JPEG 等）')
      return
    }
    try {
      const dataUrl = await readFileAsDataURL(file)
      const bytes = dataUrlBytes(dataUrl)
      if (bytes < 0) {
        toast.danger('无法识别这个文件，请换一张图片')
        return
      }
      if (bytes > MAX_LOGO_BYTES) {
        toast.danger(`LOGO 超过 ${Math.round(MAX_LOGO_BYTES / 1024)}KB，请先压缩`)
        return
      }
      setLogo(dataUrl)
    } catch (e) {
      toast.danger((e as Error).message)
    }
  }

  const save = async () => {
    const trimmed = name.trim()
    if ([...trimmed].length > MAX_NAME_RUNES) {
      toast.danger(`站点名称最多 ${MAX_NAME_RUNES} 个字`)
      return
    }
    setSaving(true)
    try {
      const body: SiteBranding = { site_name: trimmed, site_logo: logo }
      await api.saveSite(body)
      // 立刻写进共享 store：侧栏品牌不用刷页就变；登录页下次进也读这份。
      setBranding({ name: trimmed || DEFAULT_SITE_NAME, logo })
      toast.success('站点设置已保存')
      onSaved?.()
    } catch (e) {
      toast.danger('保存失败：' + (e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <SectionCard
      title="站点标识"
      description="显示在登录页上方与侧栏左上角。留空则使用默认名称与首字母方块。"
    >
      <div className="flex max-w-[680px] flex-col gap-4">
        <div className="flex items-center gap-3">
          <BrandMark size={44} />
          <div className="text-muted text-[11px] leading-relaxed">
            当前效果预览
            <br />
            {logo ? '已设置 LOGO' : '未设置 LOGO，显示站点名首字'}
          </div>
        </div>

        <TextField aria-label="站点名称" value={name} onChange={setName}>
          <Label>站点名称</Label>
          <Input placeholder={DEFAULT_SITE_NAME} autoComplete="off" />
          <span className="text-muted text-[11px]">
            最多 {MAX_NAME_RUNES} 个字；留空则显示「{DEFAULT_SITE_NAME}」
          </span>
        </TextField>

        <div className="flex flex-col gap-1">
          <span className="text-[13px] font-medium">LOGO</span>
          <div className="flex flex-wrap items-center gap-2">
            {/* 原生文件输入只能由系统对话框触发，全仓统一封装在 ui/FileImport
                （反手写门禁的白名单只认那一个文件），这里只拿 onPick 回调。 */}
            <FileImport accept="image/*" label="选择图片" onPick={(f) => void pickLogo(f)} />
            {logo ? (
              <Button variant="outline" size="sm" onPress={() => setLogo('')}>
                移除 LOGO
              </Button>
            ) : null}
          </div>
          <span className="text-muted text-[11px]">
            支持 PNG / JPEG / SVG 等，{Math.round(MAX_LOGO_BYTES / 1024)}KB 以内；横版 LOGO 不会被裁切
          </span>
        </div>

        <div>
          <Button variant="primary" size="sm" isDisabled={saving || loading} onPress={() => void save()}>
            {saving ? '保存中…' : '保存站点设置'}
          </Button>
        </div>
      </div>
    </SectionCard>
  )
}
