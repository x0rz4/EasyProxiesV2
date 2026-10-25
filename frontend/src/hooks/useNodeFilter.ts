import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { fetchNodeFacets, listSavedNodeFilters, queryNodeView } from '../api/client'
import type { NodeFilterSpec, NodeViewPage } from '../types/nodeView'

interface NodeQueryScope {
  runtimeOnly?: boolean
  healthyOnly?: boolean
}

export function useNodeFacets({ runtimeOnly = false, healthyOnly = false }: NodeQueryScope = {}) {
  return useQuery({ queryKey: ['nodeFacets', runtimeOnly, healthyOnly], queryFn: ({ signal }) => fetchNodeFacets(signal, runtimeOnly, healthyOnly), refetchInterval: 10_000 })
}

export function useNodeFilter(initial: NodeFilterSpec = { version: 2 }, enabled = true, scope: NodeQueryScope = {}) {
  const [spec, updateSpec] = useState(initial)
  const [page, setPage] = useState<NodeViewPage>({ offset: 0, limit: 50, sort: 'name', descending: false })
  const facets = useNodeFacets(scope)
  const saved = useQuery({ queryKey: ['savedNodeFilters'], queryFn: listSavedNodeFilters })
  // Page scope is applied after saved/cleared user filters, so it cannot widen.
  const querySpec = { ...spec, ...(scope.runtimeOnly ? { runtime_only: true } : {}),
    ...(scope.healthyOnly ? { healthy_only: true } : {}) }
  const query = useQuery({
    queryKey: ['nodeQuery', querySpec, page],
    queryFn: ({ signal }) => queryNodeView(querySpec, page, signal),
    enabled,
    refetchInterval: 10_000,
  })
  const setSpec = (next: NodeFilterSpec) => {
    updateSpec(next)
    setPage(current => ({ ...current, offset: 0 }))
  }
  return { spec, querySpec, setSpec, page, setPage, facets, saved, query }
}
