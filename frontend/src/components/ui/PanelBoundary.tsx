import { Component, type ReactNode } from 'react'

export default class PanelBoundary extends Component<{ children: ReactNode }, { failed: boolean }> {
  state = { failed: false }
  static getDerivedStateFromError() { return { failed: true } }

  render() {
    if (this.state.failed) return <div role="alert" className="m-6 rounded-xl border border-error/30 bg-error/5 p-6">
      <p className="font-semibold">页面加载失败</p>
      <p className="mt-2 text-sm text-base-content/60">可切换到其它页面；如果服务刚更新，请刷新以加载新版本。</p>
      <button className="btn btn-primary btn-sm mt-4" onClick={() => window.location.reload()}>刷新页面</button>
    </div>
    return this.props.children
  }
}
