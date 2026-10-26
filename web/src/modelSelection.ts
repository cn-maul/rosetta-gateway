/**
 * Add every visible option to the existing selection without removing any
 * hidden selection. Input order is retained, and the result is deduplicated.
 */
export function unionSelection(existing: readonly string[], visible: readonly string[]): string[] {
  return [...new Set([...existing, ...visible])]
}
