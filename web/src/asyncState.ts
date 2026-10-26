/**
 * Monotonic ownership for async view state.
 *
 * Every load starts a new generation. A result may be applied only while both
 * the generation and the selection that produced it are still current.
 */
export class RequestGeneration {
  private current = 0

  begin(): number {
    this.current += 1
    return this.current
  }

  invalidate(): number {
    return this.begin()
  }

  isCurrent(generation: number): boolean {
    return generation === this.current
  }
}

export function selectionKey(ids: readonly string[], days: number, granularity: string): string {
  return `${days}:${granularity}:${[...new Set(ids)].sort().join('\u0000')}`
}

export type LoadState = 'not-loaded' | 'loading' | 'ready' | 'error'
