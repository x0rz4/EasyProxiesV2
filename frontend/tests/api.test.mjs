import test from 'node:test'
import assert from 'node:assert/strict'
import { getEventListeners } from 'node:events'

globalThis.window = new EventTarget()
globalThis.localStorage = { removeItem() {}, getItem() { throw new Error('blocked') }, setItem() { throw new Error('blocked') } }
const { ApiError, request, authFetch, setToken, clearToken } = await import('../src/api/transport.ts')
const { EventStreamDecoder, readEvents, waitForReconnect } = await import('../src/api/sse.ts')
const { streamTraffic, streamNodeCheckTask, probeAllNodes, fetchTagAssignments, streamSpeedtest } = await import('../src/api/client.ts')
const { readPreference, writePreference } = await import('../src/utils/storage.ts')
const { createQueryClient } = await import('../src/queryClient.ts')

test('headers preserve Headers and tuple inputs; credentials are not persisted', async t => {
  t.after(clearToken)
  setToken('synthetic')
  const seen = []
  t.mock.method(globalThis, 'fetch', async (path, options) => { seen.push({ path, ...options }); return Response.json({ ok: true }) })
  for (const headers of [new Headers({ 'X-Example': 'kept' }), [['X-Example', 'kept']]]) {
    assert.deepEqual(await request('/api/example', { method: 'POST', body: '{}', headers }), { ok: true })
  }
  assert.equal(readPreference('theme'), null)
  assert.doesNotThrow(() => writePreference('theme', 'dark'))
  for (const request of seen) {
    assert.equal(request.headers.get('X-Example'), 'kept')
    assert.equal(request.headers.get('Authorization'), 'Bearer synthetic')
    assert.equal(request.headers.get('Content-Type'), 'application/json')
    assert.equal(request.credentials, 'same-origin')
    assert.equal(request.cache, 'no-store')
  }
})

test('401 invalidates the session but a late old-session 401 cannot clear a new login', async t => {
  t.after(clearToken)
  let unauthorized = 0
  const listener = () => unauthorized++
  window.addEventListener('auth:unauthorized', listener)
  t.after(() => window.removeEventListener('auth:unauthorized', listener))
  t.mock.method(globalThis, 'fetch', async () => new Response('denied', { status: 401 }))
  await assert.rejects(authFetch('/api/example'), error => error instanceof ApiError && error.status === 401)
  assert.equal(unauthorized, 1)
  let release
  globalThis.fetch = () => new Promise(resolve => { release = resolve })
  setToken('old')
  const old = authFetch('/api/example')
  setToken('new')
  release(new Response('', { status: 401 }))
  await assert.rejects(old, ApiError)
  assert.equal(unauthorized, 1)
  globalThis.fetch = async (_, options) => { assert.equal(options.headers.get('Authorization'), 'Bearer new'); return Response.json({}) }
  await authFetch('/api/example')
})

test('SSE split CRLF, comments, named events and multi-line data survive single-character chunks', () => {
  const decoder = new EventStreamDecoder()
  const input = ': heartbeat\r\nevent: progress\r\ndata:{"label":\r\ndata: "香港"}\r\n\r\ndata: {}\n\n'
  const events = [...input].flatMap(character => decoder.feed(character))
  assert.deepEqual(events, [{ type: 'progress', data: '{"label":\n"香港"}' }, { type: 'message', data: '{}' }])
  assert.throws(() => new EventStreamDecoder().feed('x'.repeat(16 * 1024 * 1024 + 1)), error => error instanceof ApiError && error.status === 413)
})

test('UTF-8 byte boundaries decode correctly and early completion releases the reader', async t => {
  let cancelled = 0
  const bytes = new TextEncoder().encode('data: {"name":"香港🇭🇰"}\n\n')
  const body = new ReadableStream({ start(controller) { for (const byte of bytes) controller.enqueue(Uint8Array.of(byte)) }, cancel() { cancelled++ } })
  t.mock.method(globalThis, 'fetch', async () => new Response(body, { headers: { 'Content-Type': 'text/event-stream' } }))
  for await (const frame of readEvents('/api/events', new AbortController())) { assert.equal(JSON.parse(frame.data).name, '香港🇭🇰'); break }
  assert.equal(cancelled, 1)
  assert.equal(body.locked, false)
})

test('abort unblocks a stalled reader and reconnect timer without leaving listeners', async t => {
  const body = new ReadableStream()
  t.mock.method(globalThis, 'fetch', async () => new Response(body, { headers: { 'Content-Type': 'text/event-stream' } }))
  const controller = new AbortController()
  const stream = readEvents('/api/events', controller)
  const pending = stream.next()
  await new Promise(resolve => setImmediate(resolve))
  const waiting = waitForReconnect(controller.signal, 60_000)
  controller.abort()
  assert.equal((await pending).done, true)
  await waiting
  assert.equal(body.locked, false)
  assert.equal(getEventListeners(controller.signal, 'abort').length, 0)
})

test('normal traffic EOF is observable; interrupted mutating probe is never replayed', async t => {
  const calls = []
  t.mock.method(globalThis, 'fetch', async (path, options) => { calls.push([path, options.method]); return new Response('', { headers: { 'Content-Type': 'text/event-stream' } }) })
  await new Promise(resolve => streamTraffic(() => assert.fail('unexpected event'), error => { assert.match(error.message, /断开/); resolve() }))
  await new Promise(resolve => probeAllNodes(() => assert.fail('unexpected event'), error => { assert.match(error.message, /提前断开/); resolve() }))
  assert.deepEqual(calls, [['/api/nodes/traffic/stream', 'GET'], ['/api/nodes/probe-all', 'POST']])
})

test('terminal node task stops immediately even if the server leaves its stream open', async t => {
  let cancelled = 0
  const body = new ReadableStream({ start(controller) { controller.enqueue(new TextEncoder().encode('data: {"type":"done"}\n\n')) }, cancel() { cancelled++ } })
  t.mock.method(globalThis, 'fetch', async () => new Response(body, { headers: { 'Content-Type': 'text/event-stream' } }))
  await new Promise(resolve => streamNodeCheckTask('fixture', event => { assert.equal(event.type, 'done'); resolve() }, assert.fail))
  await new Promise(resolve => setImmediate(resolve))
  assert.equal(cancelled, 1)
  assert.equal(body.locked, false)
})

test('node tag lookup keeps explicit empty scope and forwards cancellation', async t => {
  const controller = new AbortController()
  const paths = []
  t.mock.method(globalThis, 'fetch', async (path, options) => { paths.push(path); assert.equal(options.signal, controller.signal); return Response.json({ assignments: [] }) })
  assert.deepEqual(await fetchTagAssignments([], controller.signal), { assignments: [] })
  await fetchTagAssignments([7], controller.signal)
  await fetchTagAssignments(undefined, controller.signal)
  assert.deepEqual(paths, ['/api/tags/assignments?node_ids=7', '/api/tags/assignments'])
})

test('terminal reconnect snapshots deliver one UI completion before releasing their reader', async t => {
  for (const status of ['completed', 'failed', 'cancelled', 'interrupted']) {
    let cancelled = 0
    const body = new ReadableStream({ start(controller) { controller.enqueue(new TextEncoder().encode('data: '+JSON.stringify({ type:'task', task:{ status } })+'\n\n'+'data: {"type":"done"}\n\n')) }, cancel() { cancelled++ } })
    t.mock.method(globalThis, 'fetch', async () => new Response(body, { headers: { 'Content-Type': 'text/event-stream' } }))
    const events = []
    await new Promise(resolve => streamNodeCheckTask('fixture', event => { events.push(event.type); if (event.type === 'done') resolve() }, assert.fail))
    await new Promise(resolve => setImmediate(resolve))
    assert.deepEqual(events, ['task','done'])
    assert.equal(cancelled, 1)
    assert.equal(body.locked, false)
  }
})

test('named speed events finish and named server errors report without reconnecting', async t => {
  const calls = []
  t.mock.method(globalThis, 'fetch', async path => {
    calls.push(path)
    const data = calls.length === 1 ? 'event: progress\ndata: {"mbps":12.5}\n\nevent: done\ndata: {"mbps":10}\n\n' : 'event: error\ndata: {"error":"synthetic failure"}\n\n'
    return new Response(data, { headers: { 'Content-Type': 'text/event-stream' } })
  })
  const speeds = []
  await new Promise(resolve => streamSpeedtest('a@v1', (speed, done) => { speeds.push(speed); if (done) resolve() }, assert.fail))
  assert.deepEqual(speeds, [12.5, 10])
  await new Promise(resolve => streamSpeedtest('a@v1', assert.fail, error => { assert.equal(error.message, 'synthetic failure'); resolve() }))
  assert.equal(calls.length, 2)
})

test('query policy does not retry authorization or replay mutations; transient reads retry once', async () => {
  const client = createQueryClient()
  // Keep this behavioral test free of browser retention timers; the browser
  // smoke test separately exercises the production cache lifecycle.
  client.setDefaultOptions({ queries: { ...client.getDefaultOptions().queries, gcTime: Infinity }, mutations: { gcTime: Infinity } })
  try {
    let reads = 0
    await assert.rejects(client.fetchQuery({ queryKey: ['auth'], retryDelay: 0, queryFn() { reads++; throw new ApiError('denied', 401) } }))
    assert.equal(reads, 1)
    reads = 0
    await assert.rejects(client.fetchQuery({ queryKey: ['network'], retryDelay: 0, queryFn() { reads++; throw new Error('network') } }))
    assert.equal(reads, 2)
    let writes = 0
    const mutation = client.getMutationCache().build(client, { mutationFn() { writes++; return Promise.reject(new Error('network')) } })
    await assert.rejects(mutation.execute())
    assert.equal(writes, 1)
    client.setQueryData(['secret'], { uri: 'synthetic' })
    client.clear()
    assert.equal(client.getQueryCache().getAll().length, 0)
    assert.equal(client.getMutationCache().getAll().length, 0)
  } finally { client.clear() }
})
