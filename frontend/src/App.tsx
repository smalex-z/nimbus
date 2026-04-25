import { useEffect, useState } from 'react'
import { BrowserRouter, Route, Routes } from 'react-router-dom'
import Background from '@/components/Background'
import Layout from '@/components/Layout'
import MyVMs from '@/pages/MyVMs'
import Nodes from '@/pages/Nodes'
import Provision from '@/pages/Provision'
import Setup from '@/pages/Setup'
import { getSetupStatus } from '@/api/client'

type AppState = 'loading' | 'setup' | 'ready'

export default function App() {
  const [state, setState] = useState<AppState>('loading')

  useEffect(() => {
    getSetupStatus()
      .then((s) => setState(s.configured ? 'ready' : 'setup'))
      .catch(() => setState('setup'))
  }, [])

  if (state === 'loading') {
    return (
      <div className="min-h-screen grid place-items-center">
        <Background />
        <div className="brand-mark brand-mark-lg animate-pulse" />
      </div>
    )
  }

  if (state === 'setup') {
    return <Setup />
  }

  return (
    <BrowserRouter>
      <Background />
      <Layout>
        <Routes>
          <Route path="/" element={<Provision />} />
          <Route path="/vms" element={<MyVMs />} />
          <Route path="/nodes" element={<Nodes />} />
        </Routes>
      </Layout>
    </BrowserRouter>
  )
}
