import Fuse from 'fuse.js'
import type { FuseOptionKey } from 'fuse.js'

export interface FuzzySearchResult<T> {
  item: T
  score: number
  refIndex: number
}

export function createFuzzySearcher<T>(items: readonly T[], keys: FuseOptionKey<T>[]): Fuse<T> {
  return new Fuse(items, {
    keys,
    isCaseSensitive: false,
    includeScore: true,
    ignoreLocation: true,
    minMatchCharLength: 1,
    threshold: 0.35,
  })
}

export function searchAllTokens<T>(searcher: Fuse<T>, query: string): FuzzySearchResult<T>[] {
  const tokens = [...new Set(
    query.trim().split(/\s+/).map((token) => token.toLocaleLowerCase()).filter(Boolean),
  )]
  if (tokens.length === 0) return []

  const matches = new Map<number, { item: T; totalScore: number }>()
  tokens.forEach((token, tokenIndex) => {
    const tokenResults = searcher.search(token)
    if (tokenIndex === 0) {
      for (const result of tokenResults) {
        matches.set(result.refIndex, { item: result.item, totalScore: result.score ?? 1 })
      }
      return
    }

    const currentTokenMatches = new Map(
      tokenResults.map((result) => [result.refIndex, result.score ?? 1]),
    )
    for (const [refIndex, match] of matches) {
      const score = currentTokenMatches.get(refIndex)
      if (score === undefined) {
        matches.delete(refIndex)
      } else {
        match.totalScore += score
      }
    }
  })

  return [...matches.entries()]
    .map(([refIndex, match]) => ({
      item: match.item,
      score: match.totalScore / tokens.length,
      refIndex,
    }))
    .sort((a, b) => a.score - b.score || a.refIndex - b.refIndex)
}
