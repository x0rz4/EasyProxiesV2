import { useState } from 'react'
import { login } from '../api/client'

interface LoginPageProps {
  onLogin: () => void
}

export default function LoginPage({ onLogin }: LoginPageProps) {
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!password.trim()) {
      setError('请输入密码')
      return
    }

    setLoading(true)
    setError('')

    try {
      await login(password)
      onLogin()
    } catch (err) {
      setError(err instanceof Error ? err.message : '登录失败')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="min-h-screen w-full flex flex-col lg:flex-row bg-base-100 overflow-hidden">
      {/* Left Side: Branding / Intro */}
      <div className="lg:w-5/12 bg-primary/5 p-8 lg:p-16 xl:p-24 flex flex-col justify-between relative overflow-hidden border-b lg:border-b-0 lg:border-r border-base-300/30">
        {/* Decorative Background Elements */}
        <div className="absolute top-[-10%] left-[-10%] w-[60%] h-[60%] bg-primary/20 rounded-full blur-[100px] mix-blend-multiply opacity-60"></div>
        <div className="absolute bottom-[-10%] right-[-10%] w-[60%] h-[60%] bg-secondary/20 rounded-full blur-[100px] mix-blend-multiply opacity-60"></div>
        
        <div className="relative z-10 flex items-center gap-4">
          <div className="w-12 h-12 rounded-2xl bg-base-100 flex items-center justify-center shadow-sm border border-base-300/50">
            <svg xmlns="http://www.w3.org/2000/svg" className="h-6 w-6 text-primary" fill="none" viewBox="0 0 24 24" stroke="currentColor">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M13 10V3L4 14h7v7l9-11h-7z" />
            </svg>
          </div>
          <span className="text-2xl font-black text-primary tracking-tight">Easy Proxies</span>
        </div>

        <div className="relative z-10 mt-16 lg:mt-0 flex-1 flex flex-col justify-center">
          <h1 className="text-4xl lg:text-5xl xl:text-6xl font-black text-base-content tracking-tight leading-tight">
            高效、轻量的<br className="hidden lg:block" />
            <span className="text-primary mt-2 block">代理池管理系统</span>
          </h1>
          <p className="text-base-content/60 mt-6 text-base lg:text-lg max-w-md font-medium leading-relaxed">
            为高可用代理服务提供强劲动力。一站式解决多节点负载均衡、健康检查、区域路由与实时监控。
          </p>

          <div className="mt-12 flex flex-col gap-4">
            <div className="flex items-center gap-4 text-sm font-medium bg-base-100/60 backdrop-blur-md p-4 rounded-2xl border border-base-300/40 shadow-sm max-w-md transition-transform hover:-translate-y-1 duration-300">
               <div className="w-12 h-12 rounded-full bg-primary/10 flex items-center justify-center text-primary shrink-0">
                  <svg xmlns="http://www.w3.org/2000/svg" className="h-6 w-6" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                    <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M9 12l2 2 4-4m5.618-4.016A11.955 11.955 0 0112 2.944a11.955 11.955 0 01-8.618 3.04A12.02 12.02 0 003 9c0 5.591 3.824 10.29 9 11.622 5.176-1.332 9-6.03 9-11.622 0-1.042-.133-2.052-.382-3.016z" />
                  </svg>
               </div>
               <div>
                 <div className="text-base-content font-bold text-base">高可用架构</div>
                 <div className="text-base-content/60 text-xs mt-0.5">多重调度策略与毫秒级故障转移</div>
               </div>
            </div>

            <div className="flex items-center gap-4 text-sm font-medium bg-base-100/60 backdrop-blur-md p-4 rounded-2xl border border-base-300/40 shadow-sm max-w-md transition-transform hover:-translate-y-1 duration-300">
               <div className="w-12 h-12 rounded-full bg-secondary/10 flex items-center justify-center text-secondary shrink-0">
                  <svg xmlns="http://www.w3.org/2000/svg" className="h-6 w-6" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                    <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M3 10h18M7 15h1m4 0h1m-7 4h12a3 3 0 003-3V8a3 3 0 00-3-3H6a3 3 0 00-3 3v8a3 3 0 003 3z" />
                  </svg>
               </div>
               <div>
                 <div className="text-base-content font-bold text-base">自动化管理</div>
                 <div className="text-base-content/60 text-xs mt-0.5">订阅定时更新与 GeoIP 智能分发</div>
               </div>
            </div>
          </div>
        </div>

        <div className="relative z-10 mt-12 hidden lg:block">
          <p className="text-sm font-medium text-base-content/40">&copy; {new Date().getFullYear()} Easy Proxies. All rights reserved.</p>
        </div>
      </div>

      {/* Right Side: Login Form */}
      <div className="lg:w-7/12 p-8 lg:p-24 flex flex-col justify-center items-center bg-base-100 relative">
        <div className="max-w-md w-full">
          <div className="mb-12 text-center lg:text-left">
            <h2 className="text-3xl lg:text-4xl font-bold text-base-content mb-3 tracking-tight">欢迎回来</h2>
            <p className="text-base text-base-content/50 font-medium">请输入管理面板密码以继续使用系统</p>
          </div>

          <form onSubmit={handleSubmit} className="space-y-6">
            <fieldset className="fieldset">
              <legend className="fieldset-legend font-semibold text-base-content/70">管理密码</legend>
              <div className="relative">
                <div className="absolute inset-y-0 left-0 pl-4 flex items-center pointer-events-none text-base-content/40">
                  <svg xmlns="http://www.w3.org/2000/svg" className="h-5 w-5" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                    <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M12 15v2m-6 4h12a2 2 0 002-2v-6a2 2 0 00-2-2H6a2 2 0 00-2 2v6a2 2 0 002 2zm10-10V7a4 4 0 00-8 0v4h8z" />
                  </svg>
                </div>
                <input aria-label="管理密码"
                  type="password"
                  placeholder="请输入密码"
                  className={`input input-lg w-full pl-12 bg-base-200/50 focus:bg-base-100 transition-colors text-base ${error ? 'input-error ring-1 ring-error/50' : 'focus:ring-2 focus:ring-primary/20'}`}
                  value={password}
                  onChange={(e) => {
                    setPassword(e.target.value)
                    setError('')
                  }}
                  autoFocus
                  disabled={loading}
                />
              </div>
            </fieldset>

            {error && (
              <div role="alert" className="alert alert-error alert-soft py-3 text-sm animate-in fade-in slide-in-from-top-2 duration-300">
                <svg xmlns="http://www.w3.org/2000/svg" className="h-5 w-5 shrink-0" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                  <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M10 14l2-2m0 0l2-2m-2 2l-2-2m2 2l2 2m7-2a9 9 0 11-18 0 9 9 0 0118 0z" />
                </svg>
                <span className="font-medium">{error}</span>
              </div>
            )}

            <button
              type="submit"
              className="btn btn-primary w-full h-14 text-lg font-semibold shadow-lg shadow-primary/20 hover:shadow-primary/30 transition-all active:scale-[0.98] mt-4"
              disabled={loading}
            >
              {loading ? (
                <span className="loading loading-spinner loading-md"></span>
              ) : (
                <>
                  <span>登录系统</span>
                  <svg xmlns="http://www.w3.org/2000/svg" className="h-5 w-5 ml-1" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                    <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2.5" d="M14 5l7 7m0 0l-7 7m7-7H3" />
                  </svg>
                </>
              )}
            </button>
          </form>
          
          <div className="mt-16 text-center lg:text-left lg:hidden">
             <p className="text-xs text-base-content/30 font-medium">
               &copy; {new Date().getFullYear()} Easy Proxies. All rights reserved.
             </p>
          </div>
        </div>
      </div>
    </div>
  )
}