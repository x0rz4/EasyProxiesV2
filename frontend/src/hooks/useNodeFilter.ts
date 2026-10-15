import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { fetchNodeFacets, listSavedNodeFilters, queryNodeView } from '../api/client'
import type { NodeFilterSpec, NodeViewPage } from '../types/nodeView'

export function useNodeFacets() {
  return useQuery({ queryKey: ['nodeFacets'], queryFn: ({ signal }) => fetchNodeFacets(signal), refetchInterval: 10_000 })
}

export function useNodeFilter(initial: NodeFilterSpec = { version: 2 }, enabled = true) {
  const [spec, updateSpec] = useState(initial)
  const [page, setPage] = useState<NodeViewPage>({ offset: 0, limit: 50, sort: 'id', descending: false })
  const facets = useNodeFacets()
  const saved = useQuery({ queryKey: ['savedNodeFilters'], queryFn: listSavedNodeFilters })
  const query = useQuery({
    queryKey: ['nodeQuery', spec, page],
    queryFn: ({ signal }) => queryNodeView(spec, page, signal),
    enabled,
    refetchInterval: 10_000,
  })
  const setSpec = (next: NodeFilterSpec) => {
    updateSpec(next)
    setPage(current => ({ ...current, offset: 0 }))
  }
  return { spec, setSpec, page, setPage, facets, saved, query }
}
