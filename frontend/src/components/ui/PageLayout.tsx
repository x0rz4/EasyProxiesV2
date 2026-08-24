import type { ReactNode } from 'react'

interface PageLayoutProps {
  children: ReactNode
  fill?: boolean
}

interface PageHeaderProps {
  title: string
  description: ReactNode
  icon: ReactNode
  actions?: ReactNode
  sticky?: boolean
}

interface PageContentProps {
  children: ReactNode
  fill?: boolean
  className?: string
}

export const surfaceClass = 'rounded-2xl border border-base-300/50 bg-base-100 shadow-sm'
export const controlClass = 'bg-base-200/50 transition-colors focus:border-primary/50 focus:bg-base-100'

export function PageLayout({ children, fill = false }: PageLayoutProps) {
  return (
    <div className={`flex min-h-0 flex-col animate-in fade-in duration-500 ${fill ? 'h-[calc(100vh-4rem)]' : 'min-h-full'}`}>
      {children}
    </div>
  )
}

export function PageHeader({ title, description, icon, actions, sticky = true }: PageHeaderProps) {
  return (
    <header className={`${sticky ? 'sticky top-0 z-30' : 'shrink-0'} border-b border-base-300/60 bg-base-100/85 px-4 py-4 shadow-sm backdrop-blur-xl lg:px-8`}>
      <div className="mx-auto flex w-full max-w-[1600px] items-center justify-between gap-3 sm:gap-6">
        <div className="min-w-0 shrink">
          <h2 className="flex items-center gap-2.5 whitespace-nowrap text-xl font-bold sm:gap-3 sm:text-2xl">
            <span className="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl border border-primary/20 bg-primary/10 text-primary sm:h-10 sm:w-10">
              {icon}
            </span>
            {title}
          </h2>
          <div className="ml-[3.25rem] mt-1.5 hidden truncate text-sm text-base-content/50 sm:block">{description}</div>
        </div>
        {actions && <div className="flex shrink-0 flex-nowrap items-center justify-end gap-2">{actions}</div>}
      </div>
    </header>
  )
}

export function PageContent({ children, fill = false, className = '' }: PageContentProps) {
  return (
    <main className={`mx-auto w-full max-w-[1600px] p-4 lg:p-8 ${fill ? 'flex min-h-0 flex-1 flex-col' : 'flex-1 space-y-6 pb-10'} ${className}`}>
      {children}
    </main>
  )
}
