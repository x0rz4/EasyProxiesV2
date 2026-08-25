import React, { useState, useEffect, useCallback } from 'react'
import MonitorPanel from './components/MonitorPanel'
import ManagePanel from './components/ManagePanel'
import DebugPanel from './components/DebugPanel'
import SettingsPanel from './components/SettingsPanel'
import SubscriptionsPanel from './components/SubscriptionsPanel'
import UnlockPanel from './components/UnlockPanel'
import LoginPage from './components/LoginPage'
import { checkAuth, getToken, logout } from './api/client'
import packageJson from '../package.json'

type AuthState = 'loading' | 'need_login' | 'authenticated'
type TabId = 'monitor' | 'manage' | 'subscriptions' | 'unlock' | 'debug' | 'settings'

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
    icon: (
      <svg xmlns="http://www.w3.org/2000/svg" className="h-5 w-5" fill="none" viewBox="0 0 24 24" stroke="currentColor">
        <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M9 19v-6a2 2 0 00-2-2H5a2 2 0 00-2 2v6a2 2 0 002 2h2a2 2 0 002-2zm0 0V9a2 2 0 012-2h2a2 2 0 012 2v10m-6 0a2 2 0 002 2h2a2 2 0 002-2m0 0V5a2 2 0 012-2h2a2 2 0 012 2v14a2 2 0 01-2 2h-2a2 2 0 01-2-2z" />
      </svg>
    ),
  },
  {
    id: 'subscriptions',
    label: '订阅管理',
    desc: '订阅源与同步',
    icon: (
      <svg xmlns="http://www.w3.org/2000/svg" className="h-5 w-5" fill="none" viewBox="0 0 24 24" stroke="currentColor">
        <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M13.828 10.172a4 4 0 00-5.656 0l-4 4a4 4 0 105.656 5.656l1.102-1.101m-.758-4.899a4 4 0 005.656 0l4-4a4 4 0 00-5.656-5.656l-1.1 1.1" />
      </svg>
    ),
  },
  {
    id: 'manage',
    label: '节点管理',
    desc: '操作与配置',
    icon: (
      <svg xmlns="http://www.w3.org/2000/svg" className="h-5 w-5" fill="none" viewBox="0 0 24 24" stroke="currentColor">
        <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M4 6h16M4 10h16M4 14h16M4 18h16" />
      </svg>
    ),
  },
  {
    id: 'unlock',
    label: '解锁检测',
    desc: '流媒体/AI 鉴定',
    icon: (
      <svg xmlns="http://www.w3.org/2000/svg" className="h-5 w-5" fill="none" viewBox="0 0 24 24" stroke="currentColor">
        <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M8 11V7a4 4 0 118 0m-4 8v2m-6 4h12a2 2 0 002-2v-6a2 2 0 00-2-2H6a2 2 0 00-2 2v6a2 2 0 002 2z" />
      </svg>
    ),
  },
  {
    id: 'debug',
    label: '调试面板',
    desc: '运行时信息',
    icon: (
      <svg xmlns="http://www.w3.org/2000/svg" className="h-5 w-5" fill="none" viewBox="0 0 24 24" stroke="currentColor">
        <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M10 20l4-16m4 4l4 4-4 4M6 16l-4-4 4-4" />
      </svg>
    ),
  },
  {
    id: 'settings',
    label: '系统设置',
    desc: '系统运行配置',
    icon: (
      <svg xmlns="http://www.w3.org/2000/svg" className="h-5 w-5" fill="none" viewBox="0 0 24 24" stroke="currentColor">
        <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M10.325 4.317c.426-1.756 2.924-1.756 3.35 0a1.724 1.724 0 002.573 1.066c1.543-.94 3.31.826 2.37 2.37a1.724 1.724 0 001.065 2.572c1.756.426 1.756 2.924 0 3.35a1.724 1.724 0 00-1.066 2.573c.94 1.543-.826 3.31-2.37 2.37a1.724 1.724 0 00-2.572 1.065c-.426 1.756-2.924 1.756-3.35 0a1.724 1.724 0 00-2.573-1.066c-1.543.94-3.31-.826-2.37-2.37a1.724 1.724 0 00-1.065-2.572c-1.756-.426-1.756-2.924 0-3.35a1.724 1.724 0 001.066-2.573c-.94-1.543.826-3.31 2.37-2.37.996.608 2.296.07 2.572-1.065z" />
        <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M15 12a3 3 0 11-6 0 3 3 0 016 0z" />
      </svg>
    ),
  },
]

const VALID_TABS: TabId[] = ['monitor', 'manage', 'subscriptions', 'unlock', 'debug', 'settings']
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
  const saved = localStorage.getItem(THEME_STORAGE_KEY)
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
  const [activeTab, setActiveTab] = useState<TabId>(getTabFromHash)
  const [authState, setAuthState] = useState<AuthState>('loading')
  const [theme, setTheme] = useState(getInitialTheme)
  const [sidebarOpen, setSidebarOpen] = useState(false)

  // Sync hash → state on browser back/forward
  useEffect(() => {
    const onHashChange = () => setActiveTab(getTabFromHash())
    window.addEventListener('hashchange', onHashChange)
    return () => window.removeEventListener('hashchange', onHashChange)
  }, [])

  useEffect(() => {
    document.documentElement.setAttribute('data-theme', theme)
    localStorage.setItem(THEME_STORAGE_KEY, theme)
  }, [theme])

  useEffect(() => {
    const doCheck = async () => {
      try {
        const res = await checkAuth()
        if (res.no_password) {
          setAuthState('authenticated')
        } else if (getToken()) {
          setAuthState('authenticated')
        } else {
          setAuthState('need_login')
        }
      } catch {
        if (getToken()) {
          setAuthState('authenticated')
        } else {
          setAuthState('need_login')
        }
      }
    }
    doCheck()
  }, [])

  useEffect(() => {
    const handler = () => setAuthState('need_login')
    window.addEventListener('auth:unauthorized', handler)
    return () => window.removeEventListener('auth:unauthorized', handler)
  }, [])

  const handleLogin = useCallback(() => setAuthState('authenticated'), [])
  const handleLogout = useCallback(() => {
    logout()
    setAuthState('need_login')
  }, [])

  const handleTabClick = useCallback((tab: TabId) => {
    setActiveTab(tab)
    window.location.hash = tab
    setSidebarOpen(false)
  }, [])

  const renderContent = () => {
    switch (activeTab) {
      case 'monitor': return <MonitorPanel />
      case 'manage': return <ManagePanel />
      case 'subscriptions': return <SubscriptionsPanel />
      case 'unlock': return <UnlockPanel />
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

  return (
    <div className="flex flex-col h-screen overflow-hidden bg-base-100">
      {/* ===== Top Navbar ===== */}
      <div className="navbar bg-base-100/95 backdrop-blur-xl z-50 min-h-[4rem] h-16 px-4 lg:px-6 shrink-0 border-b border-base-300/40 shadow-sm sticky top-0">
        <div className="flex-none lg:hidden mr-2">
          <button
            className="btn btn-square btn-ghost btn-sm rounded-lg"
            onClick={() => setSidebarOpen(!sidebarOpen)}
            aria-label="toggle sidebar"
          >
            <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" className="inline-block w-5 h-5 stroke-current">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2.5" d="M4 6h16M4 12h16M4 18h16" />
            </svg>
          </button>
        </div>

        {/* Logo Area */}
        <div className="flex-1 lg:flex-none lg:w-64 px-2 flex items-center gap-3.5 border-r-0 lg:border-r border-base-300/30">
          <div className="w-10 h-10 rounded-2xl bg-gradient-to-br from-primary/20 to-primary/5 flex items-center justify-center shadow-sm border border-primary/10">
            <svg xmlns="http://www.w3.org/2000/svg" className="h-5 w-5 text-primary" fill="none" viewBox="0 0 24 24" stroke="currentColor">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2.5" d="M13 10V3L4 14h7v7l9-11h-7z" />
            </svg>
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
            <div tabIndex={0} role="button" className="btn btn-ghost btn-sm h-9 px-3 gap-2 normal-case rounded-lg border border-base-300/50 hover:border-primary/30 hover:bg-primary/5 transition-all">
              <svg xmlns="http://www.w3.org/2000/svg" className="h-4 w-4 shrink-0" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M7 21a4 4 0 01-4-4V5a2 2 0 012-2h4a2 2 0 012 2v12a4 4 0 01-4 4zm0 0h12a2 2 0 002-2v-4a2 2 0 00-2-2h-2.343M11 7.343l1.657-1.657a2 2 0 012.828 0l2.829 2.829a2 2 0 010 2.828l-8.486 8.485M7 17h.01" />
              </svg>
              <div className="hidden sm:flex items-center gap-1.5">
                <span className="text-xs capitalize font-medium">{theme}</span>
                <span className="badge badge-ghost badge-xs">{getThemeProfile(theme).tag}</span>
              </div>
              <svg xmlns="http://www.w3.org/2000/svg" className="h-3 w-3 opacity-60" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M19 9l-7 7-7-7" />
              </svg>
            </div>
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
                          <svg xmlns="http://www.w3.org/2000/svg" className="h-4 w-4 text-primary shrink-0" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2.5" d="M5 13l4 4L19 7" />
                          </svg>
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
            title="退出登录"
          >
            <svg xmlns="http://www.w3.org/2000/svg" className="h-5 w-5" fill="none" viewBox="0 0 24 24" stroke="currentColor">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M17 16l4-4m0 0l-4-4m4 4H7m6 4v1a3 3 0 01-3 3H6a3 3 0 01-3-3V7a3 3 0 013-3h4a3 3 0 013 3v1" />
            </svg>
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
          className={`
            fixed lg:static inset-y-0 left-0 z-40
            w-64 bg-base-100/95 backdrop-blur-xl border-r border-base-300/40
            transform transition-transform duration-300 ease-in-out
            ${sidebarOpen ? 'translate-x-0 shadow-2xl' : '-translate-x-full'}
            lg:translate-x-0 lg:shrink-0 lg:shadow-none
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
                <span className="text-sm font-semibold text-base-content/80">系统运行正常</span>
                <span className="text-xs text-base-content/40">{APP_VERSION}</span>
              </div>
            </div>
          </div>
        </aside>

        {/* Main Content Area */}
        <main className="flex-1 overflow-y-auto bg-base-200/40 relative">
          {renderContent()}
        </main>
      </div>

    </div>
  )
}

export default App
