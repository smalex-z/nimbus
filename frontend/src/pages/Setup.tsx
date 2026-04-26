import { useEffect, useState } from 'react'
import Background from '@/components/Background'
import Card from '@/components/ui/Card'
import Button from '@/components/ui/Button'
import Input from '@/components/ui/Input'
import {
  getSetupStatus,
  testProxmoxConnection,
  saveSetupConfig,
  discoverProxmox,
  createAdminAccount,
  type SaveConfigRequest,
  type DiscoverResult,
} from '@/api/client'

type Step = 'proxmox' | 'network' | 'admin' | 'review' | 'restarting'

interface ProxmoxFields {
  host: string
  tokenId: string
  tokenSecret: string
}

interface NetworkFields {
  ipPoolStart: string
  ipPoolEnd: string
  gatewayIp: string
  nameserver: string
  searchDomain: string
  port: string
  gopherApiUrl: string
  gopherApiKey: string
}

interface AdminFields {
  name: string
  email: string
  password: string
}

export default function Setup() {
  const [step, setStep] = useState<Step>('proxmox')
  const [proxmox, setProxmox] = useState<ProxmoxFields>({
    host: '',
    tokenId: 'root@pam!nimbus',
    tokenSecret: '',
  })
  const [network, setNetwork] = useState<NetworkFields>({
    ipPoolStart: '',
    ipPoolEnd: '',
    gatewayIp: '',
    nameserver: '',
    searchDomain: '',
    port: '',
    gopherApiUrl: '',
    gopherApiKey: '',
  })
  const [admin, setAdmin] = useState<AdminFields>({ name: '', email: '', password: '' })
  const [discovery, setDiscovery] = useState<DiscoverResult | null>(null)
  const [discovering, setDiscovering] = useState(true)
  const [hostAutofilled, setHostAutofilled] = useState(false)
  const [gatewayAutofilled, setGatewayAutofilled] = useState(false)
  const [testing, setTesting] = useState(false)
  const [testOk, setTestOk] = useState<string | null>(null)
  const [testError, setTestError] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const [saveError, setSaveError] = useState<string | null>(null)

  useEffect(() => {
    discoverProxmox()
      .then((d) => {
        setDiscovery(d)
        if (d.is_hypervisor && d.endpoints.length > 0) {
          setProxmox((p) => {
            if (p.host) return p
            setHostAutofilled(true)
            return { ...p, host: d.endpoints[0] }
          })
        }
        if (d.suggested_gateway) {
          setNetwork((n) => {
            if (n.gatewayIp) return n
            setGatewayAutofilled(true)
            return { ...n, gatewayIp: d.suggested_gateway! }
          })
        }
      })
      .catch(() => setDiscovery({ is_hypervisor: false, endpoints: [] }))
      .finally(() => setDiscovering(false))
  }, [])

  const updateProxmox = (key: keyof ProxmoxFields, value: string) => {
    setProxmox((p) => ({ ...p, [key]: value }))
    setTestOk(null)
    setTestError(null)
  }

  const updateAdmin = (key: keyof AdminFields, value: string) => {
    setAdmin((a) => ({ ...a, [key]: value }))
  }

  const handleTest = async () => {
    setTesting(true)
    setTestOk(null)
    setTestError(null)
    try {
      const res = await testProxmoxConnection({
        proxmox_host: proxmox.host,
        proxmox_token_id: proxmox.tokenId,
        proxmox_token_secret: proxmox.tokenSecret,
      })
      setTestOk(`Connected — Proxmox VE ${res.proxmox_version}`)
    } catch (err) {
      setTestError(err instanceof Error ? err.message : 'connection failed')
    } finally {
      setTesting(false)
    }
  }

  const handleSave = async () => {
    setSaving(true)
    setSaveError(null)
    const req: SaveConfigRequest = {
      proxmox_host: proxmox.host,
      proxmox_token_id: proxmox.tokenId,
      proxmox_token_secret: proxmox.tokenSecret,
      ip_pool_start: network.ipPoolStart,
      ip_pool_end: network.ipPoolEnd,
      gateway_ip: network.gatewayIp,
    }
    if (network.nameserver) req.nameserver = network.nameserver
    if (network.searchDomain) req.search_domain = network.searchDomain
    if (network.port) req.port = network.port
    if (network.gopherApiUrl) req.gopher_api_url = network.gopherApiUrl
    if (network.gopherApiKey) req.gopher_api_key = network.gopherApiKey
    try {
      await saveSetupConfig(req)
      setStep('restarting')
    } catch (err) {
      setSaveError(err instanceof Error ? err.message : 'save failed')
      setSaving(false)
    }
  }

  if (step === 'restarting') {
    return <RestartingView admin={admin} />
  }

  const wizardSteps = [
    { key: 'proxmox' as const, label: 'Proxmox' },
    { key: 'network' as const, label: 'Network' },
    { key: 'admin' as const, label: 'Admin account' },
    { key: 'review' as const, label: 'Review' },
  ]
  const stepIndex = wizardSteps.findIndex((s) => s.key === step)

  return (
    <div className="min-h-screen flex flex-col">
      <Background />

      {/* Minimal header */}
      <div
        className="sticky top-0 z-50 border-b border-line"
        style={{
          backdropFilter: 'blur(20px) saturate(140%)',
          WebkitBackdropFilter: 'blur(20px) saturate(140%)',
          background: 'rgba(255,255,255,0.75)',
        }}
      >
        <div className="max-w-[720px] mx-auto px-8 py-4 flex items-center justify-between">
          <div className="flex items-center gap-2.5">
            <div className="brand-mark" />
            <span className="font-display font-semibold text-xl tracking-tight">Nimbus</span>
          </div>
          <div className="font-mono text-xs text-ink-3 tracking-widest uppercase">
            Setup wizard
          </div>
        </div>
      </div>

      <main
        className={`flex-1 mx-auto w-full px-8 py-12 animate-fadeIn ${
          step === 'admin' ? 'max-w-[1080px]' : 'max-w-[720px]'
        }`}
      >
        {/* Step indicator */}
        <div className="flex items-center gap-3 mb-10 flex-wrap">
          {wizardSteps.map((s, i) => (
            <div key={s.key} className="flex items-center gap-3">
              <div
                className={`w-6 h-6 rounded-full flex items-center justify-center text-xs font-mono font-medium transition-colors ${
                  i < stepIndex
                    ? 'bg-ink text-white'
                    : i === stepIndex
                      ? 'bg-ink text-white'
                      : 'bg-[rgba(27,23,38,0.07)] text-ink-3'
                }`}
              >
                {i < stepIndex ? '✓' : i + 1}
              </div>
              <span
                className={`text-sm font-medium ${
                  i === stepIndex ? 'text-ink' : 'text-ink-3'
                }`}
              >
                {s.label}
              </span>
              {i < wizardSteps.length - 1 && <div className="w-8 h-px bg-line-2" />}
            </div>
          ))}
        </div>

        {step === 'proxmox' && (
          <ProxmoxStep
            fields={proxmox}
            onChange={updateProxmox}
            onTest={handleTest}
            testing={testing}
            testOk={testOk}
            testError={testError}
            discovery={discovery}
            discovering={discovering}
            hostAutofilled={hostAutofilled}
            onDismissHostHint={() => setHostAutofilled(false)}
            onNext={() => setStep('network')}
          />
        )}
        {step === 'network' && (
          <NetworkStep
            fields={network}
            onChange={(key, val) => {
              if (key === 'gatewayIp') setGatewayAutofilled(false)
              setNetwork((n) => ({ ...n, [key]: val }))
            }}
            gatewayAutofilled={gatewayAutofilled}
            onBack={() => setStep('proxmox')}
            onNext={() => setStep('admin')}
          />
        )}
        {step === 'admin' && (
          <AdminStep
            fields={admin}
            onChange={updateAdmin}
            onBack={() => setStep('network')}
            onNext={() => setStep('review')}
          />
        )}
        {step === 'review' && (
          <ReviewStep
            proxmox={proxmox}
            network={network}
            admin={admin}
            onBack={() => setStep('admin')}
            onSave={handleSave}
            saving={saving}
            saveError={saveError}
          />
        )}
      </main>
    </div>
  )
}

// ── Step 1: Proxmox ──────────────────────────────────────────────────────────

interface ProxmoxStepProps {
  fields: ProxmoxFields
  onChange: (key: keyof ProxmoxFields, value: string) => void
  onTest: () => void
  testing: boolean
  testOk: string | null
  testError: string | null
  discovery: DiscoverResult | null
  discovering: boolean
  hostAutofilled: boolean
  onDismissHostHint: () => void
  onNext: () => void
}

function ProxmoxStep({ fields, onChange, onTest, testing, testOk, testError, discovery, discovering, hostAutofilled, onDismissHostHint, onNext }: ProxmoxStepProps) {
  const canNext = !!fields.host && !!fields.tokenId && !!fields.tokenSecret && !!testOk
  const [guideOpen, setGuideOpen] = useState(false)

  return (
    <div>
      <div className="eyebrow">Step 1 of 4</div>
      <h2 className="text-3xl mt-1 mb-2">Connect to Proxmox</h2>
      <p className="text-base text-ink-2 leading-relaxed mb-6">
        Nimbus needs an API token to talk to your Proxmox cluster. Enter the URL of any
        cluster node — Nimbus discovers the rest automatically.
      </p>

      <Card className="p-8 flex flex-col gap-5">
        <Input
          label="Proxmox API URL"
          labelAddon={
            hostAutofilled ? (
              <AutofillBadge message="API URL auto-detected — this machine is a Proxmox node. Verify it looks correct before continuing." />
            ) : undefined
          }
          placeholder="https://192.168.0.1:8006"
          value={fields.host}
          onChange={(e) => {
            onDismissHostHint()
            onChange('host', e.target.value)
          }}
          hint="Any node in your cluster — include https:// and port 8006. Self-signed TLS certs are accepted."
        />
        {discovering ? (
          <div className="flex items-center gap-1.5 text-[11px] text-ink-3 -mt-2">
            <span className="w-1 h-1 rounded-full bg-ink-3 animate-pulse inline-block" />
            Scanning for PVE nodes…
          </div>
        ) : discovery && discovery.endpoints.length > 0 ? (
          <div className="flex flex-wrap items-center gap-1.5 -mt-2">
            <span className="text-[11px] text-ink-3 font-mono">detected:</span>
            {discovery.endpoints.map((ep) => (
              <button
                key={ep}
                onClick={() => onChange('host', ep)}
                className={`font-mono text-[11px] px-2 py-0.5 rounded-[5px] border transition-colors cursor-pointer ${
                  fields.host === ep
                    ? 'bg-ink text-white border-ink'
                    : 'bg-transparent border-line-2 text-ink-2 hover:border-ink-3 hover:text-ink'
                }`}
              >
                {ep}
              </button>
            ))}
          </div>
        ) : null}
        <Input
          label="Token ID"
          placeholder="root@pam!nimbus"
          value={fields.tokenId}
          onChange={(e) => onChange('tokenId', e.target.value)}
          hint="Format: user@realm!tokenname — e.g. root@pam!nimbus. See the guide below if you haven't created one yet."
        />
        <Input
          label="Token secret"
          type="password"
          placeholder="xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
          value={fields.tokenSecret}
          onChange={(e) => onChange('tokenSecret', e.target.value)}
          hint="The UUID shown once when you created the token. Lost it? Delete the token in the Proxmox UI and create a new one."
        />

        <div className="flex items-center gap-3 mt-1">
          <Button
            variant="ghost"
            onClick={onTest}
            disabled={!fields.host || !fields.tokenId || !fields.tokenSecret || testing}
          >
            {testing ? 'Testing…' : 'Test connection'}
          </Button>
          {testOk && (
            <span className="text-sm text-good flex items-center gap-1.5">
              <span className="w-1.5 h-1.5 rounded-full bg-good inline-block" />
              {testOk}
            </span>
          )}
          {testError && <span className="text-sm text-bad">{testError}</span>}
        </div>
      </Card>

      {/* Inline token creation guide */}
      <div className="mt-4 rounded-[10px] border border-line-2 overflow-hidden">
        <button
          className="w-full flex items-center justify-between px-5 py-3.5 text-sm font-medium text-ink hover:bg-[rgba(27,23,38,0.03)] transition-colors text-left"
          onClick={() => setGuideOpen((o) => !o)}
        >
          <span className="flex items-center gap-2">
            <span className="text-ink-3">?</span>
            How to create a Proxmox API token
          </span>
          <span className="text-ink-3 text-xs">{guideOpen ? '▲' : '▼'}</span>
        </button>

        {guideOpen && (
          <div className="px-5 pb-5 border-t border-line bg-[rgba(27,23,38,0.02)]">
            <ol className="mt-4 flex flex-col gap-3 text-sm text-ink-2">
              <GuideStep n={1}>
                Open the Proxmox web UI in your browser at{' '}
                <span className="font-mono text-xs bg-[rgba(27,23,38,0.06)] px-1.5 py-0.5 rounded">
                  https://&lt;your-node-ip&gt;:8006
                </span>{' '}
                and log in.
              </GuideStep>
              <GuideStep n={2}>
                In the left sidebar, click{' '}
                <strong className="text-ink">Datacenter</strong> (the top-level item, not a
                specific node).
              </GuideStep>
              <GuideStep n={3}>
                Go to{' '}
                <span className="font-mono text-xs bg-[rgba(27,23,38,0.06)] px-1.5 py-0.5 rounded">
                  Permissions → API Tokens
                </span>{' '}
                in the submenu, then click <strong className="text-ink">Add</strong>.
              </GuideStep>
              <GuideStep n={4}>
                Set <strong className="text-ink">User</strong> to{' '}
                <code className="font-mono text-xs bg-[rgba(27,23,38,0.06)] px-1.5 py-0.5 rounded">
                  root@pam
                </code>{' '}
                and <strong className="text-ink">Token ID</strong> to{' '}
                <code className="font-mono text-xs bg-[rgba(27,23,38,0.06)] px-1.5 py-0.5 rounded">
                  nimbus
                </code>
                .
              </GuideStep>
              <GuideStep n={5} highlight>
                <strong>Uncheck "Privilege Separation".</strong> Without this, the token
                has no effective permissions and all API calls will fail with 403.
              </GuideStep>
              <GuideStep n={6}>
                Click <strong className="text-ink">Add</strong>. Copy the token secret from
                the popup immediately — Proxmox will not show it again. If you miss it,
                delete the token and create a new one.
              </GuideStep>
              <GuideStep n={7}>
                Paste the Token ID (
                <code className="font-mono text-xs bg-[rgba(27,23,38,0.06)] px-1.5 py-0.5 rounded">
                  root@pam!nimbus
                </code>
                ) and secret into the fields above, then click{' '}
                <strong className="text-ink">Test connection</strong>.
              </GuideStep>
            </ol>
          </div>
        )}
      </div>

      <div className="flex justify-end mt-6">
        <Button onClick={onNext} disabled={!canNext}>
          Next: Network →
        </Button>
      </div>
      {!testOk && fields.tokenSecret && (
        <p className="text-xs text-ink-3 text-right mt-2">
          Test the connection before continuing.
        </p>
      )}
    </div>
  )
}

function AutofillBadge({ message }: { message: string }) {
  return (
    <span className="relative group inline-flex items-center">
      <span className="inline-flex items-center gap-1 text-[10px] font-mono font-medium text-[#2563EB] bg-[rgba(59,130,246,0.08)] border border-[rgba(59,130,246,0.18)] px-1.5 py-0.5 rounded-full cursor-default select-none leading-none">
        ↳ auto-filled
      </span>
      <span className="pointer-events-none absolute bottom-full left-0 mb-2 w-64 px-3 py-2.5 rounded-[8px] bg-[#1b1726] text-white text-[11px] leading-relaxed opacity-0 group-hover:opacity-100 transition-opacity duration-150 z-20 shadow-lg">
        {message}
        <span className="absolute top-full left-4 block w-2 h-2 bg-[#1b1726] rotate-45 -mt-1" />
      </span>
    </span>
  )
}

function GuideStep({
  n,
  children,
  highlight = false,
}: {
  n: number
  children: React.ReactNode
  highlight?: boolean
}) {
  return (
    <li
      className={`flex gap-3 ${highlight ? 'p-3 rounded-[8px] bg-[rgba(184,101,15,0.06)] border border-[rgba(184,101,15,0.2)] text-warn' : ''}`}
    >
      <span
        className={`flex-shrink-0 w-5 h-5 rounded-full flex items-center justify-center text-[10px] font-mono font-medium mt-0.5 ${
          highlight ? 'bg-warn text-white' : 'bg-[rgba(27,23,38,0.07)] text-ink-3'
        }`}
      >
        {n}
      </span>
      <span className="leading-relaxed">{children}</span>
    </li>
  )
}

// ── Step 2: Network ──────────────────────────────────────────────────────────

interface NetworkStepProps {
  fields: NetworkFields
  onChange: (key: keyof NetworkFields, value: string) => void
  gatewayAutofilled: boolean
  onBack: () => void
  onNext: () => void
}

function NetworkStep({ fields, onChange, gatewayAutofilled, onBack, onNext }: NetworkStepProps) {
  const canNext = !!fields.ipPoolStart && !!fields.ipPoolEnd && !!fields.gatewayIp
  const missing = !canNext && (fields.ipPoolStart || fields.ipPoolEnd || fields.gatewayIp)
  return (
    <div>
      <div className="eyebrow">Step 2 of 4</div>
      <h2 className="text-3xl mt-1 mb-2">Network</h2>
      <p className="text-base text-ink-2 leading-relaxed mb-8">
        Nimbus allocates a static IP from this pool for every VM it provisions. Pick a
        contiguous range of unused addresses on your LAN — nothing outside this range will
        ever be touched.
      </p>

      {/* Required fields */}
      <div className="mb-2 flex items-center gap-2">
        <span className="text-[11px] font-mono font-medium uppercase tracking-widest text-ink">
          Required
        </span>
        <span className="text-bad text-sm leading-none">*</span>
      </div>
      <Card className="p-8 flex flex-col gap-5 mb-6 border-[1.5px] border-line-2">
        <div className="grid grid-cols-2 gap-4">
          <Input
            label="Pool start IP *"
            placeholder="192.168.0.100"
            value={fields.ipPoolStart}
            onChange={(e) => onChange('ipPoolStart', e.target.value)}
            hint="First IP Nimbus may assign. Must be inside your LAN subnet and not already in use."
          />
          <Input
            label="Pool end IP *"
            placeholder="192.168.0.200"
            value={fields.ipPoolEnd}
            onChange={(e) => onChange('ipPoolEnd', e.target.value)}
            hint="Last IP in the range (inclusive). A /24 gives you up to 101 addresses between .100–.200."
          />
        </div>
        <Input
          label="Gateway IP *"
          labelAddon={
            gatewayAutofilled ? (
              <AutofillBadge message="Gateway auto-detected from this host's default route. Verify it matches your LAN router before continuing." />
            ) : undefined
          }
          placeholder="192.168.0.1"
          value={fields.gatewayIp}
          onChange={(e) => onChange('gatewayIp', e.target.value)}
          hint="Your router's LAN IP — the default route injected into every VM via cloud-init. Usually ends in .1."
        />
      </Card>

      {/* Optional fields */}
      <div className="mb-2 flex items-center gap-2">
        <span className="text-[11px] font-mono font-medium uppercase tracking-widest text-ink-3">
          Optional
        </span>
        <span className="text-[11px] text-ink-3">— safe defaults apply if left blank</span>
      </div>
      <Card className="p-8 flex flex-col gap-5">
        <div className="grid grid-cols-2 gap-4">
          <Input
            label="Gopher API URL"
            placeholder="https://gopher.example.com"
            value={fields.gopherApiUrl}
            onChange={(e) => onChange('gopherApiUrl', e.target.value)}
            hint="Optional reverse-tunnel gateway used to expose VMs at public hostnames. Leave blank to skip."
          />
          <Input
            label="Gopher API key"
            type="password"
            placeholder="Paste your Gopher API key"
            value={fields.gopherApiKey}
            onChange={(e) => onChange('gopherApiKey', e.target.value)}
            hint="Editable later from the Authentication page if you skip it now."
          />
        </div>
        <Input
          label="HTTP port"
          placeholder="8080"
          value={fields.port}
          onChange={(e) => onChange('port', e.target.value)}
          hint="Port this Nimbus server listens on. Change if 8080 is already taken on this host."
        />
        <div className="grid grid-cols-2 gap-4">
          <Input
            label="Nameserver"
            placeholder="1.1.1.1 8.8.8.8"
            value={fields.nameserver}
            onChange={(e) => onChange('nameserver', e.target.value)}
            hint="DNS resolvers injected into VMs. Space-separated. Defaults to Cloudflare + Google."
          />
          <Input
            label="Search domain"
            placeholder="local"
            value={fields.searchDomain}
            onChange={(e) => onChange('searchDomain', e.target.value)}
            hint="Appended to unqualified hostnames inside VMs (e.g. 'local' so 'myhost' resolves as 'myhost.local')."
          />
        </div>
      </Card>

      {missing && (
        <p className="text-xs text-bad mt-4 text-right">
          Fill in all required fields to continue.
        </p>
      )}

      <div className="flex justify-between mt-6">
        <Button variant="ghost" onClick={onBack}>
          ← Back
        </Button>
        <Button onClick={onNext} disabled={!canNext}>
          Next: Admin account →
        </Button>
      </div>
    </div>
  )
}

// ── Step 3: Admin account ────────────────────────────────────────────────────

interface AdminStepProps {
  fields: AdminFields
  onChange: (key: keyof AdminFields, value: string) => void
  onBack: () => void
  onNext: () => void
}

function AdminStep({ fields, onChange, onBack, onNext }: AdminStepProps) {
  const [confirmPassword, setConfirmPassword] = useState('')

  const passwordTooShort = fields.password.length > 0 && fields.password.length < 8
  const passwordMismatch = confirmPassword.length > 0 && confirmPassword !== fields.password
  const canNext =
    fields.name.trim() !== '' &&
    fields.email.trim() !== '' &&
    fields.password.length >= 8 &&
    fields.password === confirmPassword

  return (
    <div>
      <div className="eyebrow">Step 3 of 4</div>
      <h2 className="text-3xl mt-1 mb-2">Admin account</h2>
      <p className="text-base text-ink-2 leading-relaxed mb-6">
        Create the root admin account for this Nimbus instance. You can add more users after
        setup is complete.
      </p>

      <div className="grid grid-cols-1 lg:grid-cols-3 gap-6 items-start">
        <div className="lg:col-span-2">
          <Card className="p-8 flex flex-col gap-5">
            <Input
              label="Name"
              placeholder="Your name"
              value={fields.name}
              onChange={(e) => onChange('name', e.target.value)}
            />
            <Input
              label="Email"
              type="email"
              placeholder="admin@example.com"
              value={fields.email}
              onChange={(e) => onChange('email', e.target.value)}
            />
            <Input
              label="Password"
              type="password"
              placeholder="At least 8 characters"
              value={fields.password}
              onChange={(e) => onChange('password', e.target.value)}
              error={passwordTooShort ? 'Password must be at least 8 characters.' : undefined}
            />
            <Input
              label="Confirm password"
              type="password"
              placeholder="Re-enter your password"
              value={confirmPassword}
              onChange={(e) => setConfirmPassword(e.target.value)}
              error={passwordMismatch ? 'Passwords do not match.' : undefined}
            />
          </Card>
        </div>

        <div className="lg:col-span-1">
          <AccessCodePreviewPanel />
        </div>
      </div>

      <div className="flex justify-between mt-6">
        <Button variant="ghost" onClick={onBack}>
          ← Back
        </Button>
        <Button onClick={onNext} disabled={!canNext}>
          Next: Review →
        </Button>
      </div>
    </div>
  )
}

// AccessCodePreviewPanel mirrors the AccessCodePanel on the Authentication
// settings page. The actual code is auto-generated server-side once the
// admin row is created (no DB exists in setup mode), so this is purely
// informational — it shows the same shape and explains what the code is for.
function AccessCodePreviewPanel() {
  return (
    <div
      className="glass"
      style={{ padding: '24px 28px', display: 'flex', flexDirection: 'column', gap: 18 }}
    >
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
        <span style={{ fontSize: 15, fontWeight: 600, color: 'var(--ink)' }}>Access code</span>
        <span
          className="n-pill"
          style={{
            color: 'var(--ink-mute)',
            background: 'rgba(20,18,28,0.04)',
            border: '1px solid var(--line)',
          }}
        >
          preview
        </span>
      </div>

      <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-body)', lineHeight: 1.55 }}>
        Once your admin account is created, Nimbus auto-generates an 8-digit
        access code tied to it. Non-admin users must enter this code after
        signing in to reach the console.
      </p>

      <div
        style={{
          padding: '14px 16px',
          background: 'rgba(20,18,28,0.04)',
          border: '1px solid var(--line)',
          borderRadius: 10,
          fontFamily: 'Geist Mono, monospace',
          fontSize: 20,
          letterSpacing: '0.28em',
          color: 'var(--ink)',
          textAlign: 'center',
        }}
      >
        ••••••••
      </div>

      <ul
        style={{
          margin: 0,
          paddingLeft: 18,
          display: 'flex',
          flexDirection: 'column',
          gap: 6,
          fontSize: 13,
          color: 'var(--ink-body)',
          lineHeight: 1.55,
        }}
      >
        <li>Required for any user who isn't an admin to access the console.</li>
        <li>
          Bypassed for users signing in through Google OAuth from an authorized
          domain, or GitHub OAuth from an authorized organization.
        </li>
        <li>
          Viewable, copyable, and regeneratable from the
          {' '}<strong>Authentication</strong> page after setup completes.
        </li>
      </ul>
    </div>
  )
}

// ── Step 4: Review ───────────────────────────────────────────────────────────

interface ReviewStepProps {
  proxmox: ProxmoxFields
  network: NetworkFields
  admin: AdminFields
  onBack: () => void
  onSave: () => void
  saving: boolean
  saveError: string | null
}

function ReviewStep({ proxmox, network, admin, onBack, onSave, saving, saveError }: ReviewStepProps) {
  return (
    <div>
      <div className="eyebrow">Step 4 of 4</div>
      <h2 className="text-3xl mt-1 mb-2">Review & save</h2>
      <p className="text-base text-ink-2 leading-relaxed mb-8">
        Nimbus will write these values to the config file, restart, and create your admin
        account.
      </p>

      <Card className="p-8">
        <SectionLabel>Proxmox</SectionLabel>
        <ReviewRow label="API URL" value={proxmox.host} />
        <ReviewRow label="Token ID" value={proxmox.tokenId} />
        <ReviewRow label="Token secret" value={'•'.repeat(16)} />

        <SectionLabel className="mt-6">Network</SectionLabel>
        <ReviewRow label="IP pool" value={`${network.ipPoolStart} – ${network.ipPoolEnd}`} />
        <ReviewRow label="Gateway" value={network.gatewayIp} />
        <ReviewRow label="Nameserver" value={network.nameserver || '1.1.1.1 8.8.8.8'} />
        <ReviewRow label="Search domain" value={network.searchDomain || 'local'} />
        <ReviewRow label="Port" value={network.port || '8080'} />

        <SectionLabel className="mt-6">Admin account</SectionLabel>
        <ReviewRow label="Name" value={admin.name} />
        <ReviewRow label="Email" value={admin.email} />
        <ReviewRow label="Password" value={'•'.repeat(12)} />
      </Card>

      {saveError && (
        <div className="mt-4 p-3.5 rounded-[10px] bg-[rgba(184,58,58,0.06)] border border-[rgba(184,58,58,0.2)] text-bad text-sm">
          {saveError}
        </div>
      )}

      <div className="flex justify-between mt-6">
        <Button variant="ghost" onClick={onBack} disabled={saving}>
          ← Back
        </Button>
        <Button onClick={onSave} disabled={saving}>
          {saving ? 'Saving…' : 'Save & start Nimbus →'}
        </Button>
      </div>
    </div>
  )
}

function SectionLabel({ children, className = '' }: { children: React.ReactNode; className?: string }) {
  return (
    <div className={`text-[10px] font-mono uppercase tracking-widest text-ink-3 mb-3 ${className}`}>
      {children}
    </div>
  )
}

function ReviewRow({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex justify-between py-2.5 border-b border-dashed border-line text-[13px] last:border-b-0">
      <span className="text-ink-3">{label}</span>
      <span className="font-mono text-ink text-xs">{value}</span>
    </div>
  )
}

// ── Restarting ───────────────────────────────────────────────────────────────

function RestartingView({ admin }: { admin: AdminFields }) {
  useEffect(() => {
    let cancelled = false
    const poll = async () => {
      while (!cancelled) {
        await new Promise((r) => setTimeout(r, 1500))
        try {
          const status = await getSetupStatus()
          if (status.configured) {
            if (status.needs_admin_setup) {
              try {
                await createAdminAccount({
                  name: admin.name,
                  email: admin.email,
                  password: admin.password,
                })
              } catch {
                // Ignore — e.g. 409 if admin was already created on a retry
              }
            }
            window.location.replace('/')
            return
          }
        } catch {
          // server is restarting — keep polling
        }
      }
    }
    poll()
    return () => {
      cancelled = true
    }
  }, [admin])

  return (
    <div className="min-h-screen flex flex-col">
      <Background />
      <div className="flex-1 grid place-items-center">
        <Card className="w-full max-w-[480px] p-12 text-center mx-4">
          <div className="brand-mark brand-mark-lg mx-auto animate-pulse" />
          <div className="eyebrow mt-7">One moment</div>
          <h2 className="text-3xl mt-1">Starting Nimbus…</h2>
          <p className="text-base text-ink-2 mt-4 leading-relaxed">
            Configuration saved. The server is restarting — you'll be redirected automatically.
          </p>
        </Card>
      </div>
    </div>
  )
}

