/**
 * Internal wire protocol identifiers stay lowercase in API payloads. User-facing
 * surfaces use the matching title-case labels.
 */
export const PROTOCOL_OPTIONS = [
  { value: 'chat', label: 'Chat' },
  { value: 'response', label: 'Response' },
  { value: 'anthropic', label: 'Anthropic' },
] as const

const LABELS: Record<string, string> = {
  'chat': 'Chat',
  'response': 'Response',
  anthropic: 'Anthropic',
  auto: 'Auto',
}

export function protocolLabel(protocol: string, empty = '—'): string {
  return LABELS[protocol] ?? (protocol || empty)
}
