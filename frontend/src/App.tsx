import React, { lazy, Suspense, useState, useEffect, useCallback } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { 
  Activity, Cloud, Server, ShieldCheck, TerminalSquare, Settings, 
  Menu, Hexagon, Palette, ChevronDown, Check, LogOut, Layers3, Tags, Wrench, HeartPulse
} from 'lucide-react'
import LoginPage from './components/LoginPage'
import PanelBoundary from './components/ui/PanelBoundary'
import { checkAuth, logout } from './api/client'
import { readPreference, writePreference } from './utils/storage'
import { allowNavigation } from './hooks/useUnsavedChanges'
import packageJson from '../package.json'

const MonitorPanel = lazy(() => import('./components/MonitorPanel'))
const ManagePanel = lazy(() => import('./components/ManagePanel'))
const DebugPanel = lazy(() => import('./components/DebugPanel'))
const SettingsPanel = lazy(() => import('./components/SettingsPanel'))
const SubscriptionsPanel = lazy(() => import('./components/SubscriptionsPanel'))
const UnlockPanel = lazy(() => import('./components/UnlockPanel'))
const GroupPoolsPanel = lazy(() => import('./components/GroupPoolsPanel'))
const OperationsPanel = lazy(() => import('./components/OperationsPanel'))
const TagsPanel = lazy(() => import('./components/TagsPanel'))
const NodeMaintenancePanel = lazy(() => import('./components/NodeMaintenancePanel'))

type AuthState = 'loading' | 'need_login' | 'authenticated' | 'error'
type TabId = 'monitor' | 'groups' | 'manage' | 'maintenance' | 'tags' | 'subscriptions' | 'unlock' | 'operations' | 'debug' | 'settings'

const ALL_THEMES = [
  'light', 'dark', 'cupcake', 'bumblebee', 'emerald', 'corporate',
  'synthwave', 'retro', 'cyberpunk', 'valentine', 'halloween', 'garden',
  'forest', 'aqua', 'lofi', 'pastel', 'fantasy', 'wireframe', 'black',
  'luxury', 'dracula', 'cmyk', 'autumn', 'business', 'acid', 'lemonade',
  'night', 'coffee', 'winter', 'dim', 'nord', 'sunset',
  'caramellatte', 'abyss', 'silk',
]

const MENU_ITEMS: { id: TabId; label: string; icon: React.ReactNode; desc: string }[] = [
  {
    id: 'monitor',
    label: '节点监控',
    desc: '数据仪表盘',
    icon: <Activity className="h-5 w-5" />,
  },
  {
    id: 'subscriptions',
    label: '订阅管理',
    desc: '订阅源与同步',
    icon: <Cloud className="h-5 w-5" />,
  },
  {
    id: 'manage',
    label: '节点管理',
    desc: '操作与配置',
    icon: <Server className="h-5 w-5" />,
  },
  {
    id: 'maintenance',
    label: '节点维护',
    desc: '离线节点清理',
    icon: <HeartPulse className="h-5 w-5" />,
  },
  {
    id: 'tags',
    label: '节点标签',
    desc: '打标规则与互斥组',
    icon: <Tags className="h-5 w-5" />,
  },
  {
		id: 'groups',
		label: '分组池',
		desc: '独立端口与热替换',
		icon: <Layers3 className="h-5 w-5" />,
	},
	{
    id: 'unlock',
    label: '解锁检测',
    desc: '流媒体/AI 鉴定',
    icon: <ShieldCheck className="h-5 w-5" />,
  },
  {
    id: 'operations',
    label: '运维管理',
    desc: '探测调度与运行状态',
    icon: <Wrench className="h-5 w-5" />,
  },
  {
    id: 'debug',
    label: '调试面板',
    desc: '运行时信息',
    icon: <TerminalSquare className="h-5 w-5" />,
  },
  {
    id: 'settings',
    label: '系统设置',
    desc: '系统运行配置',
    icon: <Settings className="h-5 w-5" />,
  },
]

const VALID_TABS: TabId[] = ['monitor', 'groups', 'manage', 'maintenance', 'tags', 'subscriptions', 'unlock', 'operations', 'debug', 'settings']
const THEME_STORAGE_KEY = 'ep-theme'

const APP_VERSION = `v${packageJson.version}`

const DARK_THEMES = new Set([
  'dark', 'synthwave', 'halloween', 'forest', 'black', 'luxury',
  'dracula', 'business', 'night', 'coffee', 'dim', 'sunset', 'abyss',
])

const VIBRANT_THEMES = new Set([
  'cupcake', 'bumblebee', 'retro', 'cyberpunk', 'valentine', 'aqua',
  'pastel', 'fantasy', 'cmyk', 'acid', 'lemonade', 'caramellatte', 'silk',
])

function getThemeProfile(themeName: string): { tag: string; icon: string; desc: string } {
  if (DARK_THEMES.has(themeName)) {
    return { tag: '深色', icon: '🌙', desc: '低亮度 · 护眼' }
  }
  if (VIBRANT_THEMES.has(themeName)) {
    return { tag: '鲜艳', icon: '🎨', desc: '高对比 · 活力' }
  }
  return { tag: '清爽', icon: '☀️', desc: '中性 · 易读' }
}

function getSystemTheme(): string {
  if (window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches) {
    return 'dark'
  }
  return 'light'
}

function getInitialTheme(): string {
  const saved = readPreference(THEME_STORAGE_KEY)
  if (saved && ALL_THEMES.includes(saved)) {
    return saved
  }
  return getSystemTheme()
}

function getTabFromHash(): TabId {
  const hash = window.location.hash.replace('#', '')
  if (VALID_TABS.includes(hash as TabId)) {
    return hash as TabId
  }
  return 'monitor'
}

function App() {
  const queryClient = useQueryClient()
  const [activeTab, setActiveTab] = useState<TabId>(getTabFromHash)
  const [authState, setAuthState] = useState<AuthState>('loading')
  const [theme, setTheme] = useState(getInitialTheme)
  const [sidebarOpen, setSidebarOpen] = useState(false)
  const [authAttempt, setAuthAttempt] = useState(0)
  const [loggingOut, setLoggingOut] = useState(false)

  // Sync hash → state on browser back/forward
  useEffect(() => {
    const onHashChange = () => {
      const next = getTabFromHash()
      if (next !== activeTab && !allowNavigation()) {
        window.history.replaceState(null, '', `${window.location.pathname}${window.location.search}#${activeTab}`)
        return
      }
      setActiveTab(next)
    }
    window.addEventListener('hashchange', onHashChange)
    return () => window.removeEventListener('hashchange', onHashChange)
  }, [activeTab])

  useEffect(() => {
    document.documentElement.setAttribute('data-theme', theme)
    writePreference(THEME_STORAGE_KEY, theme)
  }, [theme])

  useEffect(() => {
    const controller = new AbortController()
    const doCheck = async () => {
      try {
        const res = await checkAuth(controller.signal)
        if (!controller.signal.aborted) setAuthState(res.no_password || res.authenticated ? 'authenticated' : 'need_login')
      } catch {
        if (!controller.signal.aborted) setAuthState('error')
      }
    }
    void doCheck()
    return () => controller.abort()
  }, [authAttempt])

  useEffect(() => {
    const handler = () => {
      void queryClient.cancelQueries()
      queryClient.clear()
      setAuthState('need_login')
    }
    window.addEventListener('auth:unauthorized', handler)
    return () => window.removeEventListener('auth:unauthorized', handler)
  }, [queryClient])

  useEffect(() => {
    if (!sidebarOpen) return
    const closeOnEscape = (event: KeyboardEvent) => { if (event.key === 'Escape') setSidebarOpen(false) }
    window.addEventListener('keydown', closeOnEscape)
    return () => window.removeEventListener('keydown', closeOnEscape)
  }, [sidebarOpen])

  const handleLogin = useCallback(() => setAuthState('authenticated'), [])
  const handleLogout = async () => {
    if (!allowNavigation()) return
    setLoggingOut(true)
    try {
      await logout()
      await queryClient.cancelQueries()
      queryClient.clear()
      setAuthState('need_login')
    } catch (error) {
      toast.error(error instanceof Error ? error.message : '退出失败，请重试')
    } finally { setLoggingOut(false) }
  }

  const handleTabClick = useCallback((tab: TabId) => {
    window.location.hash = tab
    setSidebarOpen(false)
  }, [])

  const renderContent = () => {
    switch (activeTab) {
      case 'monitor': return <MonitorPanel />
      case 'manage': return <ManagePanel />
    case 'maintenance': return <NodeMaintenancePanel />
      case 'tags': return <TagsPanel />
      case 'groups': return <GroupPoolsPanel />
      case 'subscriptions': return <SubscriptionsPanel />
      case 'unlock': return <UnlockPanel />
      case 'operations': return <OperationsPanel />
      case 'debug': return <DebugPanel />
      case 'settings': return <SettingsPanel />
      default: return <MonitorPanel />
    }
  }

  if (authState === 'loading') {
    return (
      <div className="min-h-screen flex flex-col items-center justify-center bg-base-200 gap-4">
        <span className="text-2xl font-black text-primary">Easy Proxies</span>
        <span className="loading loading-spinner loading-lg text-primary"></span>
      </div>
    )
  }

  if (authState === 'need_login') {
    return <LoginPage onLogin={handleLogin} />
  }

  if (authState === 'error') return <div role="alert" className="min-h-screen flex flex-col items-center justify-center gap-4 bg-base-200">
    <p>无法连接管理服务，请检查网络后重试。</p>
    <button className="btn btn-primary" onClick={() => { setAuthState('loading'); setAuthAttempt(n => n + 1) }}>重新连接</button>
  </div>

  return (
    <div className="flex flex-col h-screen overflow-hidden bg-base-100">
      {/* ===== Top Navbar ===== */}
      <div className="navbar bg-base-100/95 backdrop-blur-xl z-50 min-h-[4rem] h-16 px-4 lg:px-6 shrink-0 border-b border-base-300/40 shadow-sm sticky top-0">
        <div className="flex-none lg:hidden mr-2">
          <button
            className="btn btn-square btn-ghost btn-sm rounded-lg"
            onClick={() => setSidebarOpen(!sidebarOpen)}
            aria-label="切换导航菜单"
            aria-expanded={sidebarOpen}
            aria-controls="main-navigation"
          >
            <Menu className="w-5 h-5" />
          </button>
        </div>

        {/* Logo Area */}
        <div className="flex-1 lg:flex-none lg:w-64 px-2 flex items-center gap-3.5 border-r-0 lg:border-r border-base-300/30">
          <div className="w-10 h-10 rounded-2xl bg-gradient-to-br from-primary/20 to-primary/5 flex items-center justify-center shadow-sm border border-primary/10">
            <Hexagon className="h-5 w-5 text-primary" />
          </div>
          <div>
            <div className="font-black text-lg text-base-content tracking-tight leading-none mb-1">
              Easy Proxies
            </div>
            <div className="text-[11px] font-medium text-base-content/40 tracking-wider uppercase">
              Dashboard
            </div>
          </div>
        </div>

        {/* Top Right Actions */}
        <div className="flex-none flex items-center gap-2 lg:gap-3 ml-auto">
          {/* Theme Selector */}
          <div className="dropdown dropdown-end">
            <button type="button" aria-label="选择主题" className="btn btn-ghost btn-sm h-9 px-3 gap-2 normal-case rounded-lg border border-base-300/50 hover:border-primary/30 hover:bg-primary/5 transition-all">
              <Palette className="h-4 w-4 shrink-0" />
              <div className="hidden sm:flex items-center gap-1.5">
                <span className="text-xs capitalize font-medium">{theme}</span>
                <span className="badge badge-ghost badge-xs">{getThemeProfile(theme).tag}</span>
              </div>
              <ChevronDown className="h-3 w-3 opacity-60" />
            </button>
            <ul tabIndex={0} className="dropdown-content bg-base-200 border border-base-300/50 rounded-box shadow-2xl mt-2 p-2.5 w-80 max-h-96 overflow-y-auto z-[100] space-y-1">
              {ALL_THEMES.map((t) => {
                const profile = getThemeProfile(t)
                const isActive = theme === t
                return (
                  <li key={t}>
                    <button
                      className={`w-full rounded-xl px-2 py-2 border transition-all duration-150 ${
                        isActive
                          ? 'bg-primary/10 border-primary/30 shadow-sm'
                          : 'border-transparent hover:border-base-300/70 hover:bg-base-300/40'
                      }`}
                      onClick={() => {
                        setTheme(t)
                        const el = document.activeElement as HTMLElement
                        el?.blur()
                      }}
                    >
                      <div className="flex items-center gap-3">
                        <div className="shrink-0 rounded-lg border border-base-300/60 p-1 bg-base-100/50" data-theme={t}>
                          <div className="w-16 h-10 rounded-md bg-base-100 border border-base-300/70 p-1.5 flex flex-col justify-between">
                            <div className="h-1.5 w-9 rounded bg-primary"></div>
                            <div className="flex items-center gap-1">
                              <span className="w-2.5 h-2.5 rounded bg-secondary"></span>
                              <span className="w-2.5 h-2.5 rounded bg-accent"></span>
                              <span className="w-2.5 h-2.5 rounded bg-neutral"></span>
                              <span className="w-2.5 h-2.5 rounded bg-info"></span>
                            </div>
                          </div>
                        </div>

                        <div className="min-w-0 flex-1 text-left">
                          <div className={`text-sm font-semibold capitalize leading-tight ${isActive ? 'text-primary' : ''}`}>{t}</div>
                          <div className="mt-1 flex items-center gap-1.5 text-[11px] text-base-content/55">
                            <span>{profile.icon}</span>
                            <span>{profile.tag}</span>
                            <span className="opacity-40">·</span>
                            <span className="truncate">{profile.desc}</span>
                          </div>
                        </div>

                        {isActive && (
                          <Check className="h-4 w-4 text-primary shrink-0" strokeWidth={3} />
                        )}
                      </div>
                    </button>
                  </li>
                )
              })}
            </ul>
          </div>

          <div className="w-px h-6 bg-base-300/50 mx-1"></div>

          <button
            className="btn btn-ghost btn-sm h-9 w-9 p-0 rounded-lg hover:bg-error/10 hover:text-error transition-colors"
            onClick={handleLogout}
            disabled={loggingOut}
            aria-label="退出登录"
            title="退出登录"
          >
            <LogOut className="h-4 w-4" />
          </button>
        </div>
      </div>

      {/* ===== Body: Sidebar + Content ===== */}
      <div className="flex flex-1 overflow-hidden relative">
        {/* Mobile overlay */}
        {sidebarOpen && (
          <div
            className="fixed inset-0 bg-black/50 z-30 lg:hidden backdrop-blur-sm"
            onClick={() => setSidebarOpen(false)}
          />
        )}

        {/* Sidebar */}
        <aside
          id="main-navigation"
          className={`
            fixed lg:static inset-y-0 left-0 z-40
            w-64 bg-base-100/95 backdrop-blur-xl border-r border-base-300/40
            transform transition-transform duration-300 ease-in-out
            ${sidebarOpen ? 'translate-x-0 shadow-2xl visible' : '-translate-x-full invisible'}
            lg:translate-x-0 lg:shrink-0 lg:shadow-none lg:visible
            flex flex-col
            pt-16 lg:pt-0
          `}
        >
          {/* Navigation */}
          <nav className="flex-1 p-4 lg:p-5 overflow-y-auto">
            <div className="text-[11px] font-bold text-base-content/40 uppercase tracking-widest mb-3 px-2">主要导航</div>
            <ul className="space-y-1.5">
              {MENU_ITEMS.map((item) => {
                const isActive = activeTab === item.id;
                return (
                  <li key={item.id}>
                    <button
                      className={`w-full flex items-center gap-3.5 px-3 py-3 rounded-xl transition-all duration-200 group
                        ${isActive
                          ? 'bg-primary/10 shadow-sm border border-primary/20'
                          : 'hover:bg-base-200/50 border border-transparent'
                        }
                      `}
                      onClick={() => handleTabClick(item.id)}
                    >
                      <div className={`p-2 rounded-lg transition-colors ${
                        isActive
                          ? 'bg-primary text-primary-content shadow-md shadow-primary/20'
                          : 'bg-base-200 text-base-content/50 group-hover:bg-base-300 group-hover:text-base-content'
                      }`}>
                        {item.icon}
                      </div>
                      <div className="text-left flex-1 min-w-0">
                        <div className={`text-sm truncate ${isActive ? 'font-bold text-primary' : 'font-medium text-base-content/80 group-hover:text-base-content'}`}>
                          {item.label}
                        </div>
                        <div className={`text-xs truncate ${isActive ? 'text-primary/70' : 'text-base-content/40'}`}>
                          {item.desc}
                        </div>
                      </div>
                      {isActive && (
                        <div className="w-1.5 h-6 rounded-full bg-primary shrink-0 opacity-80"></div>
                      )}
                    </button>
                  </li>
                )
              })}
            </ul>
          </nav>

          {/* Sidebar Footer */}
          <div className="p-5 border-t border-base-300/30 bg-base-200/20 m-2 rounded-2xl mb-4">
            <div className="flex items-center gap-3">
              <div className="relative flex h-3 w-3 shrink-0">
                <span className="animate-ping absolute inline-flex h-full w-full rounded-full bg-success opacity-75"></span>
                <span className="relative inline-flex rounded-full h-3 w-3 bg-success"></span>
              </div>
              <div className="flex flex-col">
                <span className="text-sm font-semibold text-base-content/80">管理控制台</span>
                <span className="text-xs text-base-content/40">{APP_VERSION} · <a className="link" href="/FONT-LICENSE.txt">字体许可</a></span>
              </div>
            </div>
          </div>
        </aside>

        {/* Main Content Area */}
        <main className="flex-1 overflow-y-auto bg-base-200/40 relative">
          <PanelBoundary key={activeTab}>
            <Suspense fallback={<div role="status" className="flex h-64 items-center justify-center gap-3"><span className="loading loading-spinner text-primary" />正在加载页面</div>}>
              {renderContent()}
            </Suspense>
          </PanelBoundary>
        </main>
      </div>

    </div>
  )
}

export default App
