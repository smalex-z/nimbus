import { useAuth } from '@/hooks/useAuth'
import GPU from '@/pages/GPU'
import GPUHost from '@/pages/GPUHost'

// GPUPlane unifies the two GPU surfaces under a single page so the
// admin doesn't have to ping-pong between /gpu and /infrastructure/gpu-hosts.
// The operational view (jobs + inference status) renders for everyone;
// the hardware/pairing section is admin-only since it can mint pairing
// tokens and unpair the GX10.
//
// Each child page owns its own header — keeping them as standalone
// pages too made the merge a one-line composition rather than a
// refactor. The two headers read as section titles inside the unified
// view, which is acceptable for the consolidation pass; if we want a
// single page-level title later, lift each child's body into a body
// component and provide one wrapper header here.
export default function GPUPlane() {
  const { user } = useAuth()
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 40 }}>
      <GPU />
      {user?.is_admin && <GPUHost />}
    </div>
  )
}
