import type { CcSwitchApp } from '../../importLinks'
import codexIcon from '../../assets/icons/codex.svg'
import claudeIcon from '../../assets/icons/claude.svg'
import opencodeIcon from '../../assets/icons/opencode.svg'
import hermesIcon from '../../assets/icons/hermes.png'
import ccSwitchIcon from '../../assets/icons/cc-switch.png'
import magpieIcon from '../../assets/icons/magpie.svg'

/** CC Switch 可导入的客户端清单（自定义协议目标各不相同）。 */
export const CC_APPS: { id: CcSwitchApp; name: string; icon: string; detail: string; invertDark?: boolean }[] = [
  { id: 'codex', name: 'Codex', icon: codexIcon, detail: 'OpenAI Responses', invertDark: true },
  { id: 'claude', name: 'Claude Code', icon: claudeIcon, detail: 'Anthropic Messages' },
  { id: 'opencode', name: 'OpenCode', icon: opencodeIcon, detail: 'OpenAI Chat' },
  { id: 'hermes', name: 'Hermes', icon: hermesIcon, detail: 'OpenAI Chat' },
]

export { ccSwitchIcon, magpieIcon }

export type ImportTool = 'ccswitch' | 'magpie'
