import { Component, type ErrorInfo, type ReactNode } from 'react'

// ErrorBoundary catches React render exceptions so they surface as a
// readable error UI instead of a blank white page (the default React
// behavior when a render throws and no boundary is in scope). Wrap the
// whole app — every page mount goes through here.
//
// On catch we log to console (so devtools shows the stack) and render
// a small reset card. "Reload" hard-refreshes; "Home" navigates to /.
// State doesn't reset on its own — once the boundary trips, anything
// rendered under it stays errored until the next navigation or reload.
//
// Class component because React's error-boundary contract still requires
// componentDidCatch / getDerivedStateFromError. There is no hook
// equivalent.
interface Props {
  children: ReactNode
}

interface State {
  error: Error | null
}

export default class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null }

  static getDerivedStateFromError(error: Error): State {
    return { error }
  }

  componentDidCatch(error: Error, info: ErrorInfo): void {
    console.error('Nimbus UI render error:', error, info.componentStack)
  }

  render(): ReactNode {
    if (!this.state.error) return this.props.children
    const message = this.state.error.message || 'Unknown render error'
    return (
      <div className="min-h-screen grid place-items-center p-6">
        <div className="max-w-md w-full p-6 rounded-[12px] bg-white/90 border border-line shadow">
          <div className="text-[10px] font-mono uppercase tracking-widest text-ink-3">
            Render error
          </div>
          <h2 className="text-2xl mt-1">Something broke in the UI.</h2>
          <p className="text-sm text-ink-2 mt-3 leading-relaxed">
            The page hit a render exception. Devtools console has the stack.
          </p>
          <pre className="mt-4 text-xs font-mono bg-[rgba(27,23,38,0.04)] border border-line rounded p-3 overflow-auto max-h-40">
            {message}
          </pre>
          <div className="flex gap-2 mt-5">
            <button
              type="button"
              onClick={() => window.location.reload()}
              className="px-3 py-1.5 rounded bg-ink text-white text-sm hover:bg-ink/85"
            >
              Reload
            </button>
            <button
              type="button"
              onClick={() => {
                window.location.href = '/'
              }}
              className="px-3 py-1.5 rounded border border-line text-sm hover:bg-[rgba(27,23,38,0.04)]"
            >
              Home
            </button>
          </div>
        </div>
      </div>
    )
  }
}
