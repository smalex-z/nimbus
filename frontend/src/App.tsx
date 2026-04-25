import { BrowserRouter, Route, Routes } from 'react-router-dom'
import Background from '@/components/Background'
import Layout from '@/components/Layout'
import Admin from '@/pages/Admin'
import MyVMs from '@/pages/MyVMs'
import Nodes from '@/pages/Nodes'
import Provision from '@/pages/Provision'

export default function App() {
  return (
    <BrowserRouter>
      <Background />
      <Layout>
        <Routes>
          <Route path="/" element={<Provision />} />
          <Route path="/vms" element={<MyVMs />} />
          <Route path="/nodes" element={<Nodes />} />
          {/* TODO: gate behind <RequireAdmin> once auth is implemented */}
          <Route path="/admin" element={<Admin />} />
        </Routes>
      </Layout>
    </BrowserRouter>
  )
}
