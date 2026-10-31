import { useBranding } from './branding'

/**
 * 站点标志。有 LOGO 就显示图片，没有就退回「首字母方块」——
 * 这样全新部署（没配 LOGO）也有一眼能认的标记，不会出现空白方块。
 *
 * size 用像素传：侧栏 36、登录页 40，两处视觉重量接近。
 */
export function BrandMark({ size = 36 }: { size?: number }) {
  const { name, logo } = useBranding()
  const style = { width: size, height: size }
  if (logo) {
    return (
      <img
        src={logo}
        alt={name}
        style={style}
        // object-contain：用户可能传方形或横版 LOGO，横版不能被裁掉两边。
        className="shrink-0 rounded-[10px] object-contain"
      />
    )
  }
  return (
    <div
      style={style}
      aria-hidden="true"
      className="bg-accent text-accent-foreground grid shrink-0 place-items-center rounded-[10px] font-bold"
    >
      {/* 取站点名的首字符：中文取第一个字，英文取首字母大写。 */}
      {(name.trim()[0] ?? 'R').toUpperCase()}
    </div>
  )
}
