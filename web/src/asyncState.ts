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

