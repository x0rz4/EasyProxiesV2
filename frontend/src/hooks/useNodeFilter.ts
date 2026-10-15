import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { fetchNodeFacets, listSavedNodeFilters, queryNodeView } from '../api/client'
import type { NodeFilterSpec, NodeViewPage } from '../types/nodeView'

export function useNodeFacets(runtimeOnly = false) {
  return useQuery({ queryKey: ['nodeFacets', runtimeOnly], queryFn: ({ signal }) => fetchNodeFacets(signal, runtimeOnly), refetchInterval: 10_000 })
}

export function useNodeFilter(initial: NodeFilterSpec = { version: 2 }, enabled = true, runtimeOnly = false) {
  const [spec, updateSpec] = useState(initial)
  const [page, setPage] = useState<NodeViewPage>({ offset: 0, limit: 50, sort: 'name', descending: false })
  const facets = useNodeFacets(runtimeOnly)
  const saved = useQuery({ queryKey: ['savedNodeFilters'], queryFn: listSavedNodeFilters })
  const querySpec = runtimeOnly ? { ...spec, runtime_only: true } : spec
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
