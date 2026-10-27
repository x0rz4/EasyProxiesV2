import { QueryClient } from '@tanstack/react-query'
import { ApiError } from './api/transport'

export function createQueryClient() {
  return new QueryClient({ defaultOptions: { queries: {
    staleTime: 5000,
    gcTime: 60_000,
    refetchOnWindowFocus: true,
    // Authentication/validation failures need user action. Network/server
    // failures get one retry; mutations retain their default no-retry policy.
    retry: (attempt, error) => attempt < 1 && !(error instanceof ApiError && error.status >= 400 && error.status < 500),
  } } })
}
