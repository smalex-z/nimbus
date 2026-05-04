import { useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import {
  provisionVMStreaming,
  ProvisionError,
  getBootstrapStatus,
  bootstrapTemplates,
  listKeys,
  getTunnelInfo,
  getGPUInference,
  type BootstrapResult,
  type TunnelInfo,
} from '@/api/client'
import type { GPUInferenceStatus } from '@/types'
import { useAuth } from '@/hooks/useAuth'
import Card from '@/components/ui/Card'
import Button from '@/components/ui/Button'
import Input from '@/components/ui/Input'
import Textarea from '@/components/ui/Textarea'
import TierCard from '@/components/ui/TierCard'
import RadioCard from '@/components/ui/RadioCard'
import CopyButton from '@/components/ui/CopyButton'
import KeyFileUpload from '@/components/ui/KeyFileUpload'
import { validatePrivateKey, validatePublicKey } from '@/utils/sshKey'
import { buildSSHCommand, parseTunnelURL } from '@/lib/format'
import TunnelsModal from '@/components/ui/TunnelsModal'
import {
  OS_OPTIONS,
  type OSTemplate,
  type ProvisionResult,
  type ProvisionStep,
  type SSHKey,
  TIERS,
  type TierName,
  type WorkloadType,
} from '@/types'

type ViewState = 'form' | 'loading' | 'result' | 'error'
type KeyMode = 'saved' | 'byo' | 'gen'

const TIER_ORDER: TierName[] = ['small', 'medium', 'large', 'xl']

interface FormState {
  hostname: string
  tier: TierName
  // workload starts as null (= follow tier-default). The form only
  // sets it when the operator explicitly picks a workload, so changing
  // tier still re-derives the workload until they take control.
  workload: WorkloadType | null
  os: OSTemplate
  keyMode: KeyMode
  savedKeyId: number | null
  pubKey: string
  privKey: string
  publicTunnel: boolean
  enableGPU: boolean
}

// defaultWorkloadForTier mirrors nodescore.DefaultWorkloadForTier on
// the backend. Kept inline rather than fetched so the form can drive
// the "auto-recommended" highlight without a round trip.
function defaultWorkloadForTier(tier: TierName): WorkloadType {
  if (tier === 'small' || tier === 'medium') return 'web'
  if (tier === 'xl') return 'compute'
  return 'balanced'
}

const DEFAULT_FORM: FormState = {
  hostname: '',
  tier: 'medium',
  workload: null,
  os: 'ubuntu-24.04',
  keyMode: 'gen',
  savedKeyId: null,
  pubKey: '',
  privKey: '',
  publicTunnel: false,
  enableGPU: false,
}

export default function Provision() {
  const { user } = useAuth()
  const [view, setView] = useState<ViewState>('form')
  const [form, setForm] = useState<FormState>(DEFAULT_FORM)
  const [result, setResult] = useState<ProvisionResult | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [errorStep, setErrorStep] = useState<ProvisionStep | undefined>()
  // currentStep is the most recently *completed* phase; the UI lights up
  // checkmarks up to and including it, and shows the next phase as active.
  // Undefined means "nothing completed yet" — first phase is active.
  const [currentStep, setCurrentStep] = useState<ProvisionStep | undefined>()

  const [bootstrapped, setBootstrapped] = useState<boolean | null>(null)
  const [bootstrapRunning, setBootstrapRunning] = useState(false)
  const [bootstrapResult, setBootstrapResult] = useState<BootstrapResult | null>(null)
  const [bootstrapError, setBootstrapError] = useState<string | null>(null)
  const [bootstrapElapsed, setBootstrapElapsed] = useState(0)

  // Saved-key inventory. We default the picker to the user's chosen default
  // key when the page loads with at least one saved key.
  const [savedKeys, setSavedKeys] = useState<SSHKey[]>([])

  // Tunnel availability + host preview. When tunnels are disabled (no
  // GOPHER_API_URL configured) we hide the public-access section entirely
  // rather than showing a checkbox that does nothing.
  const [tunnelInfo, setTunnelInfo] = useState<TunnelInfo | null>(null)
  useEffect(() => {
    getTunnelInfo()
      .then(setTunnelInfo)
      .catch(() => setTunnelInfo({ enabled: false, host: '' }))
  }, [])

  // GPU plane availability. The "include GX10 access" checkbox only renders
  // when an admin has paired a GX10 — pre-pairing the option means nothing.
  const [gpuInfo, setGpuInfo] = useState<GPUInferenceStatus | null>(null)
  useEffect(() => {
    getGPUInference()
      .then(setGpuInfo)
      .catch(() => setGpuInfo({ enabled: false, status: 'unconfigured' }))
  }, [])

  useEffect(() => {
    getBootstrapStatus()
      .then((s) => setBootstrapped(s.bootstrapped))
      .catch(() => setBootstrapped(false))
  }, [])

  useEffect(() => {
    listKeys()
      .then((rows) => {
        setSavedKeys(rows)
        const defaultKey = rows.find((k) => k.is_default) ?? rows[0]
        if (defaultKey) {
          setForm((prev) =>
            prev.savedKeyId === null
              ? { ...prev, keyMode: 'saved', savedKeyId: defaultKey.id }
              : prev,
          )
        }
      })
      .catch(() => {
        // Non-fatal — the form still works with BYO/Generate.
      })
  }, [])

  useEffect(() => {
    if (!bootstrapRunning) return
    const t = setInterval(() => setBootstrapElapsed((e) => e + 1), 1000)
    return () => clearInterval(t)
  }, [bootstrapRunning])

  const runBootstrap = async () => {
    setBootstrapRunning(true)
    setBootstrapError(null)
    setBootstrapResult(null)
    setBootstrapElapsed(0)
    try {
      const res = await bootstrapTemplates()
      setBootstrapResult(res)
      const status = await getBootstrapStatus()
      setBootstrapped(status.bootstrapped)
    } catch (err) {
      setBootstrapError(err instanceof Error ? err.message : 'bootstrap failed')
    } finally {
      setBootstrapRunning(false)
    }
  }

  const selectedTier = TIERS[form.tier]

  // selectedKeyHasPrivate gates the "Expose SSH publicly" checkbox: Nimbus
  // can't bootstrap the Gopher tunnel into the VM without SSHing in, which
  // needs the private half. Backend enforces the same rule with a
  // ValidationError; this computes the same answer client-side so the
  // checkbox disables before submit.
  const selectedKeyHasPrivate = useMemo(() => {
    switch (form.keyMode) {
      case 'gen':
        return true // Nimbus generates the keypair → private half always present.
      case 'byo':
        return form.privKey.trim().length > 0
      case 'saved':
        if (form.savedKeyId === null) return false
        return savedKeys.find((k) => k.id === form.savedKeyId)?.has_private_key ?? false
      default:
        return false
    }
  }, [form.keyMode, form.privKey, form.savedKeyId, savedKeys])

  // Auto-clear the public-tunnel checkbox when the underlying key loses its
  // private half (e.g. user switched from "generate" to a saved pubkey-only
  // key). Without this the checkbox stays visually checked but disabled,
  // and the user can't tell the request would be rejected.
  useEffect(() => {
    if (!selectedKeyHasPrivate && form.publicTunnel) {
      setForm((prev) => ({ ...prev, publicTunnel: false }))
    }
  }, [selectedKeyHasPrivate, form.publicTunnel])

  const canSubmit = useMemo(() => {
    if (!form.hostname || form.hostname.length === 0) return false
    if (form.tier === 'xl') return false
    if (form.keyMode === 'byo' && form.pubKey.trim().length === 0) return false
    if (form.keyMode === 'saved' && form.savedKeyId === null) return false
    return true
  }, [form])

  const updateForm = <K extends keyof FormState>(key: K, value: FormState[K]) =>
    setForm((prev) => ({ ...prev, [key]: value }))

  const reset = () => {
    setForm(DEFAULT_FORM)
    setResult(null)
    setError(null)
    setErrorStep(undefined)
    setCurrentStep(undefined)
    setView('form')
  }

  const submit = async () => {
    setError(null)
    setErrorStep(undefined)
    setCurrentStep(undefined)
    setView('loading')
    try {
      const trimmedPriv = form.keyMode === 'byo' ? form.privKey.trim() : ''
      const res = await provisionVMStreaming(
        {
          hostname: form.hostname,
          tier: form.tier,
          // Empty/null workload omitted entirely so the backend
          // applies its tier-default. Sending the explicit value when
          // the operator has overridden lets the server log the
          // origin in the pickNode line.
          workload_type: form.workload ?? undefined,
          os_template: form.os,
          ssh_key_id:
            form.keyMode === 'saved' && form.savedKeyId !== null
              ? form.savedKeyId
              : undefined,
          ssh_pubkey: form.keyMode === 'byo' ? form.pubKey.trim() : undefined,
          ssh_privkey: trimmedPriv ? trimmedPriv : undefined,
          generate_key: form.keyMode === 'gen' ? true : undefined,
          public_tunnel: form.publicTunnel ? true : undefined,
          enable_gpu: form.enableGPU ? true : undefined,
          // No subdomain — provision-time tunnels are SSH only and Gopher
          // allocates a port on the gateway. Backend defaults the tunnel
          // identifier to the VM hostname so admins don't need to think
          // about it. HTTP tunnels (which DO use subdomains via wildcard
          // DNS) are added later from the machine page.
        },
        (evt) => setCurrentStep(evt.step),
      )
      setResult(res)
      setView('result')
    } catch (err: unknown) {
      if (err instanceof ProvisionError) {
        setError(err.message)
        setErrorStep(err.failedStep)
      } else {
        setError(err instanceof Error ? err.message : 'unknown error')
      }
      setView('error')
    }
  }

  if (view === 'loading') {
    return <LoadingView hostname={form.hostname} currentStep={currentStep} />
  }
  if (view === 'result' && result) {
    return <ResultView result={result} onReset={reset} />
  }
  if (view === 'error') {
    return (
      <ErrorView
        error={error ?? 'unknown'}
        failedStep={errorStep}
        onRetry={() => setView('form')}
      />
    )
  }

  return (
    <div>
      <div className="flex items-end justify-between flex-wrap gap-4 mb-2">
        <div>
          <div className="eyebrow">New machine</div>
          <h2 className="text-3xl">What are we spinning up today?</h2>
          <p className="text-base text-ink-2 mt-2 leading-relaxed">
            Pick a size, give it a name, grab your key. Most builds finish in 90–120s.
          </p>
        </div>
      </div>

      {bootstrapped === null && (
        <div className="mt-12 grid place-items-center">
          <div className="w-4 h-4 border-[1.5px] border-ink-3 border-t-ink rounded-full animate-spin" />
        </div>
      )}

      {bootstrapped === false && !user?.is_admin && <BootstrapPending />}

      {bootstrapped === false && user?.is_admin && (
        <BootstrapGate
          running={bootstrapRunning}
          result={bootstrapResult}
          error={bootstrapError}
          elapsed={bootstrapElapsed}
          onStart={runBootstrap}
        />
      )}

      {bootstrapped === true && (
        <div className="grid grid-cols-1 lg:grid-cols-[1fr_380px] gap-8 mt-8">
          <Card className="p-9">
            <FormBody
              form={form}
              updateForm={updateForm}
              savedKeys={savedKeys}
              tunnelInfo={tunnelInfo}
              gpuInfo={gpuInfo}
              selectedKeyHasPrivate={selectedKeyHasPrivate}
            />
          </Card>

          <Card className="p-7 self-start lg:sticky lg:top-[100px]">
            <Summary
              form={form}
              savedKeys={savedKeys}
              tierLabel={`${selectedTier.cpu} / ${selectedTier.memMB / 1024} GiB`}
              diskLabel={`${selectedTier.diskGB} GiB`}
            />
            <Button
              type="button"
              onClick={submit}
              disabled={!canSubmit}
              className="w-full mt-6"
            >
              Provision machine →
            </Button>
            <p className="text-xs text-ink-3 text-center mt-3 leading-relaxed">
              Typically completes in 90–120 seconds.
            </p>
            {form.tier === 'xl' && (
              <p className="text-xs text-warn text-center mt-2">
                XL tier requires admin approval — not yet enabled.
              </p>
            )}
          </Card>
        </div>
      )}
    </div>
  )
}

// ── Bootstrap gate ────────────────────────────────────────────────────────────

interface BootstrapGateProps {
  running: boolean
  result: BootstrapResult | null
  error: string | null
  elapsed: number
  onStart: () => void
}

const OS_TEMPLATES = [
  { os: 'Ubuntu 24.04 LTS', vmid: 9000 },
  { os: 'Ubuntu 22.04 LTS', vmid: 9001 },
  { os: 'Debian 12', vmid: 9002 },
  { os: 'Debian 11', vmid: 9003 },
]

function BootstrapGate({ running, result, error, elapsed, onStart }: BootstrapGateProps) {
  const fmt = (s: number) => (s < 60 ? `${s}s` : `${Math.floor(s / 60)}m ${s % 60}s`)

  if (running) {
    return (
      <div className="mt-8 grid place-items-center min-h-[320px]">
        <Card className="w-full max-w-[540px] p-12 text-center">
          <div className="brand-mark brand-mark-lg mx-auto animate-pulse" />
          <div className="eyebrow mt-7">One-time setup</div>
          <h3 className="text-2xl mt-1">Downloading templates…</h3>
          <p className="text-base text-ink-2 mt-3 leading-relaxed">
            Fetching cloud images across all cluster nodes. Don't close this tab.
          </p>
          <div className="mt-5 font-mono text-3xl text-ink tabular-nums">{fmt(elapsed)}</div>
        </Card>
      </div>
    )
  }

  return (
    <div className="mt-8">
      <Card className="p-9">
        <div className="eyebrow">Setup required</div>
        <h3 className="text-2xl mt-1 mb-3">OS templates aren't set up yet</h3>
        <p className="text-base text-ink-2 leading-relaxed mb-7">
          Nimbus provisions VMs by cloning cloud-image templates on your Proxmox nodes.
          This one-time download (~2 GiB per OS) runs in parallel across all cluster nodes
          and takes 10–20 minutes on a typical home lab. Once done, VM provisioning is
          instant.
        </p>

        <div className="grid grid-cols-2 gap-2 mb-7">
          {OS_TEMPLATES.map(({ os, vmid }) => (
            <div
              key={vmid}
              className="flex items-center justify-between px-3.5 py-2.5 rounded-[8px] bg-[rgba(27,23,38,0.03)] border border-line-2 text-[13px]"
            >
              <span className="text-ink">{os}</span>
              <span className="font-mono text-xs text-ink-3">VMID {vmid}</span>
            </div>
          ))}
        </div>

        {error && (
          <div className="mb-5 p-3.5 rounded-[10px] bg-[rgba(184,58,58,0.06)] border border-[rgba(184,58,58,0.2)] text-bad text-sm">
            {error}
          </div>
        )}

        {result && result.failed.length > 0 && (
          <div className="mb-5">
            <div className="text-[11px] font-mono uppercase tracking-widest text-bad mb-2">
              Failed ({result.failed.length})
            </div>
            <div className="flex flex-col gap-1.5">
              {result.failed.map((item, i) => (
                <div
                  key={i}
                  className="flex items-center justify-between px-3.5 py-2.5 rounded-[8px] bg-[rgba(184,58,58,0.04)] border border-[rgba(184,58,58,0.12)] text-[13px]"
                >
                  <span className="text-ink">
                    {item.os} on {item.node}
                  </span>
                  {item.error && (
                    <span
                      className="text-bad text-xs max-w-[260px] truncate"
                      title={item.error}
                    >
                      {item.error}
                    </span>
                  )}
                </div>
              ))}
            </div>
          </div>
        )}

        <div className="flex items-center justify-between">
          <p className="text-xs text-ink-3">Estimated: 10–20 min · idempotent, safe to retry</p>
          <Button onClick={onStart}>
            {result ? 'Retry →' : 'Set up templates →'}
          </Button>
        </div>
      </Card>
    </div>
  )
}

function BootstrapPending() {
  return (
    <div className="mt-8">
      <Card className="p-9">
        <div className="eyebrow">Setup pending</div>
        <h3 className="text-2xl mt-1 mb-3">Admin access required</h3>
        <p className="text-base text-ink-2 leading-relaxed">
          Cluster templates haven't been set up yet. Ask your admin to finish
          bootstrapping the OS templates — once that's done, you'll be able to
          provision a VM here.
        </p>
      </Card>
    </div>
  )
}

// ── Form ──────────────────────────────────────────────────────────────────────

interface FormBodyProps {
  form: FormState
  updateForm: <K extends keyof FormState>(key: K, value: FormState[K]) => void
  savedKeys: SSHKey[]
  tunnelInfo: TunnelInfo | null
  gpuInfo: GPUInferenceStatus | null
  // Mirrors the backend rule that public SSH needs a private half — the
  // parent computes it once and passes it down so this component doesn't
  // duplicate the keyMode → has_private_key logic.
  selectedKeyHasPrivate: boolean
}

function FormBody({ form, updateForm, savedKeys, tunnelInfo, gpuInfo, selectedKeyHasPrivate }: FormBodyProps) {
  return (
    <div className="flex flex-col gap-6">
      <Input
        label="Hostname"
        placeholder="my-project"
        value={form.hostname}
        onChange={(e) => updateForm('hostname', e.target.value.toLowerCase())}
        suffix=".nimbus.internal"
        hint="Lowercase letters, numbers, and hyphens. Must be unique within your org."
      />

      <div className="flex flex-col gap-2">
        <label className="text-[13px] font-medium text-ink">Operating system</label>
        <select
          value={form.os}
          onChange={(e) => updateForm('os', e.target.value as OSTemplate)}
          className="w-full px-3.5 py-3 rounded-[10px] bg-white/85 font-sans text-sm text-ink border border-line-2 outline-none focus:border-ink focus:bg-white"
        >
          {OS_OPTIONS.map((opt) => (
            <option key={opt.value} value={opt.value}>
              {opt.label}
            </option>
          ))}
        </select>
      </div>

      <div className="flex flex-col gap-2">
        <label className="text-[13px] font-medium text-ink">Resource tier</label>
        <div className="grid grid-cols-2 gap-2.5">
          {TIER_ORDER.map((name) => {
            const tier = TIERS[name]
            return (
              <TierCard
                key={name}
                name={tier.name}
                cpu={tier.cpu}
                memMB={tier.memMB}
                diskGB={tier.diskGB}
                selected={form.tier === name}
                locked={name === 'xl'}
                onClick={() => updateForm('tier', name)}
              />
            )
          })}
        </div>
        <p className="text-xs text-ink-3 mt-1.5 leading-relaxed">
          XL requires admin approval. You'll be notified when it's ready.
        </p>
      </div>

      <WorkloadPicker
        tier={form.tier}
        workload={form.workload}
        onChange={(w) => updateForm('workload', w)}
      />

      <div className="flex flex-col gap-2">
        <label className="text-[13px] font-medium text-ink">SSH key</label>
        <div className="grid gap-2">
          {savedKeys.length > 0 && (
            <RadioCard
              title="Use a saved key"
              description={
                savedKeys.find((k) => k.is_default)
                  ? "Your default key is selected. You can pick a different one."
                  : "Pick from the keys you've already added."
              }
              selected={form.keyMode === 'saved'}
              onClick={() => updateForm('keyMode', 'saved')}
            />
          )}
          <RadioCard
            title="Generate one for me"
            description="We'll mint an Ed25519 keypair, vault it, and show the private key once."
            selected={form.keyMode === 'gen'}
            onClick={() => updateForm('keyMode', 'gen')}
          />
          <RadioCard
            title="Bring your own key"
            description="Paste or upload a public key. Optionally store the private half so you can download it later."
            selected={form.keyMode === 'byo'}
            onClick={() => updateForm('keyMode', 'byo')}
          />
        </div>
        {form.keyMode === 'saved' && savedKeys.length > 0 && (
          <div className="mt-3 flex flex-col gap-2">
            <select
              value={form.savedKeyId ?? ''}
              onChange={(e) => updateForm('savedKeyId', Number(e.target.value))}
              className="w-full px-3.5 py-3 rounded-[10px] bg-white/85 font-sans text-sm text-ink border border-line-2 outline-none focus:border-ink focus:bg-white"
            >
              {savedKeys.map((k) => (
                <option key={k.id} value={k.id}>
                  {k.name}
                  {k.is_default ? ' · default' : ''}
                  {k.label ? ` — ${k.label}` : ''}
                </option>
              ))}
            </select>
            <p className="text-xs text-ink-3">
              Manage saved keys on the <Link to="/keys" className="underline">Keys page</Link>.
            </p>
          </div>
        )}
        {form.keyMode === 'byo' && (
          <div className="mt-4 flex flex-col gap-5">
            <div className="flex flex-col gap-2">
              <label className="text-[13px] text-ink">
                <span className="font-semibold">Private key</span>{' '}
                <span className="text-ink-3 font-normal">(PEM or OpenSSH format) — optional</span>
              </label>
              <div className="flex items-stretch gap-3">
                <div className="flex-1 min-w-0">
                  <Textarea
                    monospace
                    placeholder={'-----BEGIN OPENSSH PRIVATE KEY-----\n…'}
                    value={form.privKey}
                    onChange={(e) => updateForm('privKey', e.target.value)}
                  />
                </div>
                <KeyFileUpload
                  maxBytes={64 * 1024}
                  sizeError="File too large — private keys are typically under 4 KB."
                  validate={validatePrivateKey}
                  onLoad={(text) => updateForm('privKey', text)}
                />
              </div>
              <p className="text-[11px] text-ink-3 leading-relaxed">
                Paste only if you'd like Nimbus to vault the key so you can re-download
                it later. Leave blank to keep the private half on your machine only — the
                public key alone is enough to log in. Stored encrypted; never leaves
                Nimbus unless you ask for it.
              </p>
            </div>

            <div className="flex flex-col gap-2">
              <label className="text-[13px] text-ink">
                <span className="font-semibold">Public key</span>{' '}
                <span className="text-ink-3 font-normal">(authorized_keys format)</span>
              </label>
              <div className="flex items-stretch gap-3">
                <div className="flex-1 min-w-0">
                  <Textarea
                    monospace
                    placeholder="ssh-ed25519 AAAA..."
                    value={form.pubKey}
                    onChange={(e) => updateForm('pubKey', e.target.value)}
                  />
                </div>
                <KeyFileUpload
                  maxBytes={16 * 1024}
                  sizeError="File too large — public keys are typically under 1 KB."
                  validate={validatePublicKey}
                  onLoad={(text) => updateForm('pubKey', text)}
                />
              </div>
            </div>
          </div>
        )}
      </div>

      <div className="flex flex-col gap-2">
        <label className="text-[13px] font-medium text-ink">Public access</label>
        <label
          className={`flex items-start gap-3 p-3.5 rounded-[10px] border border-line-2 bg-white/85 transition-colors ${
            tunnelInfo?.enabled && selectedKeyHasPrivate
              ? 'cursor-pointer hover:border-ink/40'
              : 'cursor-not-allowed opacity-60'
          }`}
        >
          <input
            type="checkbox"
            checked={form.publicTunnel}
            onChange={(e) => updateForm('publicTunnel', e.target.checked)}
            disabled={!tunnelInfo?.enabled || !selectedKeyHasPrivate}
            className="mt-0.5 w-4 h-4 accent-ink disabled:cursor-not-allowed"
          />
          <div className="flex-1">
            <div className="text-sm font-medium">Expose SSH publicly</div>
            <div className="text-xs text-ink-3 mt-0.5">
              Bootstraps a Gopher reverse tunnel to this VM's port 22. Gopher
              allocates a public port at the gateway — SSH lands at{' '}
              <span className="font-mono">{tunnelInfo?.host || 'gateway'}:&lt;port&gt;</span>.
              Subdomains are an HTTP-tunnel concept and don't apply here;
              expose web services later from the machine page.
            </div>
            {!tunnelInfo?.enabled && (
              <div className="text-xs text-warn mt-1.5">
                Tunnel integration not configured. An admin can wire it up
                from{' '}
                <Link to="/infrastructure/gopher" className="underline">Infrastructure → Gopher tunnels</Link>.
              </div>
            )}
            {tunnelInfo?.enabled && !selectedKeyHasPrivate && (
              <div className="text-xs text-warn mt-1.5">
                {form.keyMode === 'byo'
                  ? 'Paste the private half above — Nimbus needs it to SSH into the VM and run the Gopher bootstrap.'
                  : form.keyMode === 'saved'
                    ? <>This saved key has no private half on file. Pick another key or upload its private key from{' '}<Link to="/keys" className="underline">SSH keys</Link>.</>
                    : 'Pick a key with a private half or generate a new one — the Gopher bootstrap needs to SSH into the VM.'}
              </div>
            )}
          </div>
        </label>
        {form.publicTunnel && tunnelInfo?.enabled && tunnelInfo.host && (
          <div className="text-xs text-ink-3 mt-1.5 leading-relaxed">
            After the VM boots, SSH will be reachable at{' '}
            <span className="font-mono text-ink">{tunnelInfo.host}:&lt;port&gt;</span>.
            Gopher assigns the port — it'll show on the result screen.
          </div>
        )}
      </div>

      {gpuInfo?.enabled && (
        <div className="flex flex-col gap-2">
          <label className="text-[13px] font-medium text-ink">GPU access</label>
          <label className="flex items-start gap-3 p-3.5 rounded-[10px] border border-line-2 bg-white/85 cursor-pointer hover:border-ink/40 transition-colors">
            <input
              type="checkbox"
              checked={form.enableGPU}
              onChange={(e) => updateForm('enableGPU', e.target.checked)}
              className="mt-0.5 w-4 h-4 accent-ink"
            />
            <div className="flex-1">
              <div className="text-sm font-medium">Include GX10 access</div>
              <div className="text-xs text-ink-3 mt-0.5">
                Injects <span className="font-mono">OPENAI_BASE_URL</span> and a{' '}
                <span className="font-mono">gx10</span> CLI helper so this VM can
                hit the inference server and submit GPU jobs. Off by default —
                only enable for VMs that actually need it.
              </div>
            </div>
          </label>
        </div>
      )}
    </div>
  )
}

interface SummaryProps {
  form: FormState
  savedKeys: SSHKey[]
  tierLabel: string
  diskLabel: string
}

function Summary({ form, savedKeys, tierLabel, diskLabel }: SummaryProps) {
  const tunnelLabel = form.publicTunnel ? 'ssh' : 'no'
  const sshLabel = (() => {
    if (form.keyMode === 'gen') return 'generate (vaulted)'
    if (form.keyMode === 'saved') {
      const k = savedKeys.find((s) => s.id === form.savedKeyId)
      return k ? k.name : '—'
    }
    return form.privKey.trim() ? 'your key (vaulted)' : 'your key'
  })()
  return (
    <>
      <h3 className="text-xl font-semibold">Summary</h3>
      <div className="mt-4">
        <SummaryRow label="Hostname" value={form.hostname || '—'} />
        <SummaryRow label="OS" value={form.os} />
        <SummaryRow label="Tier" value={form.tier} />
        <SummaryRow label="vCPU / RAM" value={tierLabel} />
        <SummaryRow label="Disk" value={diskLabel} />
        <SummaryRow label="SSH" value={sshLabel} />
        <SummaryRow label="Public SSH" value={tunnelLabel} />
        {form.enableGPU && <SummaryRow label="GPU" value="GX10 access" />}
      </div>
    </>
  )
}

function SummaryRow({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex justify-between py-2.5 border-b border-dashed border-line text-[13px] last:border-b-0">
      <span className="text-ink-3">{label}</span>
      <span className="font-mono text-ink text-xs">{value}</span>
    </div>
  )
}

interface LoadingViewProps {
  hostname: string
  currentStep: ProvisionStep | undefined
}

// Phases shown in the checklist. Each one corresponds to a `step` ID the
// backend emits over the NDJSON stream — checkmarks light up because the
// step *finished*, not because N seconds passed. Order matches the
// backend's emit order; do not reorder without updating the service.
const PROVISION_PHASES: { step: ProvisionStep; label: string }[] = [
  { step: 'reserve_ip', label: 'Reserving IP & selecting node' },
  { step: 'clone_template', label: 'Cloning golden template' },
  { step: 'configure_vm', label: 'Configuring cloud-init & disk' },
  { step: 'start_vm', label: 'Starting VM' },
  { step: 'wait_guest_agent', label: 'Waiting for guest agent' },
]

// After this many seconds in the guest-agent phase, surface a hint that
// it's normal — first-boot agent installs can legitimately take 1–2 min.
const GUEST_AGENT_HINT_AFTER_SEC = 30

function LoadingView({ hostname, currentStep }: LoadingViewProps) {
  const [elapsed, setElapsed] = useState(0)
  // Track when each phase became active so we can show a phase-specific
  // sub-hint after a threshold (e.g. guest agent taking >30s is fine but
  // worth flagging so the user knows nothing is stuck).
  const [phaseStartedAt, setPhaseStartedAt] = useState<number>(0)
  const [activePhase, setActivePhase] = useState<ProvisionStep>('reserve_ip')

  useEffect(() => {
    const t = setInterval(() => setElapsed((e) => e + 1), 1000)
    return () => clearInterval(t)
  }, [])

  // The active phase is the one *after* the last completed step.
  useEffect(() => {
    const next = nextPhase(currentStep)
    setActivePhase((prev) => {
      if (prev !== next) setPhaseStartedAt(elapsed)
      return next
    })
    // Intentionally not depending on `elapsed` — we only want the phase-start
    // marker to update when the phase itself changes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [currentStep])

  const completedIndex = currentStep
    ? PROVISION_PHASES.findIndex((p) => p.step === currentStep)
    : -1
  const activeIndex = Math.min(completedIndex + 1, PROVISION_PHASES.length - 1)
  const allDone = currentStep === 'wait_guest_agent'

  const fmt = (s: number) => (s < 60 ? `${s}s` : `${Math.floor(s / 60)}m ${s % 60}s`)

  const inAgentWait = activePhase === 'wait_guest_agent' && !allDone
  const showAgentHint =
    inAgentWait && elapsed - phaseStartedAt >= GUEST_AGENT_HINT_AFTER_SEC

  return (
    <div className="grid place-items-center min-h-[calc(100vh-160px)]">
      <Card className="w-full max-w-[560px] p-12 text-center">
        <div className="brand-mark brand-mark-lg mx-auto animate-pulse" />
        <div className="eyebrow mt-7">Provisioning</div>
        <h2 className="text-3xl mt-1">
          Hatching{' '}
          <span className="font-mono text-[22px] bg-[rgba(27,23,38,0.06)] px-2.5 py-0.5 rounded-md">
            {hostname || 'machine'}
          </span>
        </h2>
        <p className="text-base text-ink-2 mt-4 leading-relaxed">
          Hold tight — your machine is on its way. Provisioning typically takes 90–120s.
        </p>
        <div className="mt-4 font-mono text-2xl text-ink tabular-nums">{fmt(elapsed)}</div>

        <div className="mt-7 text-left border-t border-line">
          {PROVISION_PHASES.map((phase, i) => {
            const status: ProvisionStepProps['status'] = allDone
              ? 'done'
              : i <= completedIndex
                ? 'done'
                : i === activeIndex
                  ? 'active'
                  : 'pending'
            return <ProvisionStep key={phase.step} label={phase.label} status={status} />
          })}
        </div>

        {showAgentHint && (
          <p className="mt-5 text-xs text-ink-3 leading-relaxed">
            Still waiting — first-boot guest-agent setup can take up to 2 min.
            Nothing is stuck.
          </p>
        )}
      </Card>
    </div>
  )
}

function nextPhase(completed: ProvisionStep | undefined): ProvisionStep {
  if (!completed) return PROVISION_PHASES[0].step
  const i = PROVISION_PHASES.findIndex((p) => p.step === completed)
  return PROVISION_PHASES[Math.min(i + 1, PROVISION_PHASES.length - 1)].step
}

interface ProvisionStepProps {
  label: string
  status: 'pending' | 'active' | 'done'
  meta?: string
}

function ProvisionStep({ label, status, meta }: ProvisionStepProps) {
  return (
    <div
      className={`flex items-center gap-3.5 py-3.5 border-b border-line text-sm last:border-b-0 ${
        status === 'pending' ? 'text-ink-3' : 'text-ink'
      } ${status === 'active' ? 'font-medium' : ''}`}
    >
      <span
        className={`w-[18px] h-[18px] rounded-full border-[1.5px] flex-shrink-0 grid place-items-center relative ${
          status === 'pending' ? 'border-ink-3' : 'border-ink'
        } ${status === 'done' ? 'bg-ink' : ''}`}
      >
        {status === 'done' && (
          <svg
            viewBox="0 0 12 12"
            className="w-[10px] h-[10px] text-white"
            fill="none"
            stroke="currentColor"
            strokeWidth={2}
            strokeLinecap="round"
            strokeLinejoin="round"
            aria-hidden="true"
          >
            <polyline points="2.5,6.5 5,9 9.5,3.5" />
          </svg>
        )}
        {status === 'active' && (
          <span className="absolute inset-[3px] rounded-full bg-ink animate-blink" />
        )}
      </span>
      <span>{label}</span>
      {meta && <span className="ml-auto font-mono text-[11px] text-ink-3">{meta}</span>}
    </div>
  )
}

interface ResultViewProps {
  result: ProvisionResult
  onReset: () => void
}

function ResultView({ result, onReset }: ResultViewProps) {
  const { user } = useAuth()
  const [tunnelsOpen, setTunnelsOpen] = useState(false)
  const sshCommand = buildSSHCommand(result.username, result.ip, result.key_name)
  const tunnel = result.tunnel_url ? parseTunnelURL(result.tunnel_url) : undefined
  const publicSSHCommand = tunnel
    ? buildSSHCommand(result.username, tunnel.host, result.key_name, tunnel.port)
    : undefined
  const hasWarning = Boolean(result.warning)
  const hasTunnel = Boolean(publicSSHCommand)
  const statusLabel = hasWarning ? 'MACHINE READY (UNVERIFIED)' : 'MACHINE READY'
  const statusColorClass = hasWarning
    ? 'bg-[rgba(184,101,15,0.12)] text-warn'
    : 'bg-[rgba(45,125,90,0.1)] text-good'
  const dotColorClass = hasWarning ? 'bg-warn' : 'bg-good'
  const dashboardHref = user?.is_admin ? '/admin' : '/vms'
  const dashboardLabel = user?.is_admin ? 'Back to dashboard' : 'Back to my machines'
  return (
    <div className="py-5 pb-10">
      <Card className="max-w-[720px] mx-auto p-11">
        <div
          className={`inline-flex items-center gap-2 px-3 py-1 rounded-full text-xs font-mono tracking-wide mb-4 ${statusColorClass}`}
        >
          <span className={`w-1.5 h-1.5 rounded-full ${dotColorClass}`} />
          {statusLabel}
        </div>
        <h2 className="text-3xl">{result.hostname} is live.</h2>
        <p className="text-base text-ink-2 mt-2">
          {result.ssh_private_key
            ? "Save the private key now — we won't show it again."
            : 'Use your SSH key to connect.'}
        </p>


        {hasWarning && (
          <div className="mt-5 p-4 rounded-[10px] bg-[rgba(184,101,15,0.08)] border border-[rgba(184,101,15,0.2)] text-warn text-[13px] leading-relaxed flex items-start gap-2.5">
            <span className="text-base">⚠</span>
            <div>
              <strong>Reachability not confirmed.</strong> {result.warning}
            </div>
          </div>
        )}

        {result.tunnel_error && (
          <div className="mt-5 p-4 rounded-[10px] bg-[rgba(184,101,15,0.08)] border border-[rgba(184,101,15,0.2)] text-warn text-[13px] leading-relaxed flex items-start gap-2.5">
            <span className="text-base">⚠</span>
            <div>
              <strong>Tunnel not active.</strong>{' '}
              <span className="whitespace-pre-line">{result.tunnel_error}</span>
            </div>
          </div>
        )}

        <div className="grid grid-cols-1 sm:grid-cols-2 gap-3 mt-7">
          <CredCell label="Hostname" value={result.hostname} />
          <CredCell label="Local IP" value={result.ip} />
          <CredCell label="Username" value={result.username} />
          <CredCell label="VMID / Node" value={`${result.vmid} on ${result.node}`} />
          <CredCell
            label={hasTunnel ? 'SSH (LAN)' : 'SSH command'}
            value={sshCommand}
            fullWidth
          />
        </div>

        {publicSSHCommand && tunnel && (
          <GopherTunnelBox
            host={tunnel.host}
            port={tunnel.port}
            sshCommand={publicSSHCommand}
          />
        )}

        {result.ssh_private_key && (
          <div className="mt-6">
            <div className="text-[10px] font-mono uppercase tracking-widest text-ink-3 mb-1.5">
              Private key (Ed25519)
            </div>
            <PrivateKeyDownload
              keyName={result.key_name ?? `nimbus-${result.hostname}`}
              privateKey={result.ssh_private_key}
            />

            <div className="mt-6 p-3.5 rounded-[10px] bg-[rgba(45,125,90,0.08)] border border-[rgba(45,125,90,0.2)] text-good text-[13px] leading-relaxed flex items-start gap-2.5">
              <span className="text-base">🔒</span>
              <div>
                <strong>Saved to the Nimbus vault.</strong>{' '}
                Encrypted at rest — you can re-download it from the My Machines page if
                you lose this copy.
              </div>
            </div>
          </div>
        )}

        <div className="flex gap-2.5 justify-end mt-9 flex-wrap">
          {hasTunnel && (
            <Button
              variant="ghost"
              onClick={() => setTunnelsOpen(true)}
              title="Add HTTP/TCP tunnels for services running on this VM"
            >
              🌐 Manage tunnels
            </Button>
          )}
          <Link to={dashboardHref}>
            <Button variant="ghost">{dashboardLabel}</Button>
          </Link>
          <Button onClick={onReset}>Provision another</Button>
        </div>
        {tunnelsOpen && (
          <TunnelsModal
            vmId={result.id}
            hostname={result.hostname}
            onClose={() => setTunnelsOpen(false)}
          />
        )}
      </Card>
    </div>
  )
}

interface GopherTunnelBoxProps {
  host: string
  port: number
  sshCommand: string
}

// GopherTunnelBox renders a self-contained section explaining the public
// tunnel that's been wired up via Gopher (ACM@UCLA's reverse-tunnel gateway).
// Only shown when the provision actually established the tunnel.
function GopherTunnelBox({ host, port, sshCommand }: GopherTunnelBoxProps) {
  const endpoint = `${host}:${port}`
  return (
    <div className="mt-7 p-5 rounded-[10px] bg-[rgba(45,125,90,0.06)] border border-[rgba(45,125,90,0.25)]">
      <div className="flex items-center justify-between gap-3">
        <div className="flex items-center gap-2.5">
          <span className="text-lg" aria-hidden>🌐</span>
          <span className="font-display text-base font-medium">Gopher tunnel</span>
        </div>
        <span className="font-mono text-[10px] uppercase tracking-widest text-good bg-[rgba(45,125,90,0.12)] px-2 py-0.5 rounded">
          ACTIVE
        </span>
      </div>
      <p className="text-[13px] text-ink-2 mt-2 leading-relaxed">
        SSH is exposed publicly via the Gopher reverse-tunnel gateway, so you can
        reach this machine from anywhere — no LAN required.
      </p>
      <div className="grid grid-cols-1 gap-3 mt-4">
        <div className="p-3.5 rounded-[10px] bg-white/85 border border-line">
          <div className="text-[10px] font-mono uppercase tracking-widest text-ink-3 mb-1.5">
            Routing
          </div>
          <div className="font-mono text-sm text-ink break-all flex items-center gap-2 flex-wrap">
            <span>{endpoint}</span>
            <span className="text-ink-3" aria-hidden>→</span>
            <span>localhost:22</span>
            <span className="font-mono text-[10px] uppercase tracking-widest text-ink-3 bg-[rgba(27,23,38,0.06)] px-1.5 py-0.5 rounded">
              SSH
            </span>
          </div>
        </div>
        <CredCell label="SSH (public)" value={sshCommand} fullWidth />
      </div>
    </div>
  )
}

interface CredCellProps {
  label: string
  value: string
  fullWidth?: boolean
}

function CredCell({ label, value, fullWidth = false }: CredCellProps) {
  return (
    <div
      className={`p-3.5 rounded-[10px] bg-white/85 border border-line ${
        fullWidth ? 'sm:col-span-2' : ''
      }`}
    >
      <div className="text-[10px] font-mono uppercase tracking-widest text-ink-3 mb-1.5">
        {label}
      </div>
      <div className="font-mono text-sm text-ink break-all flex items-center justify-between gap-3">
        <span>{value}</span>
        <CopyButton value={value} />
      </div>
    </div>
  )
}

interface PrivateKeyDownloadProps {
  keyName: string
  privateKey: string
}

function PrivateKeyDownload({ keyName, privateKey }: PrivateKeyDownloadProps) {
  const [downloaded, setDownloaded] = useState(false)

  const download = () => {
    // Ensure the file ends with a single trailing newline — OpenSSH expects it.
    const content = privateKey.endsWith('\n') ? privateKey : privateKey + '\n'
    const blob = new Blob([content], { type: 'application/x-pem-file' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = keyName
    document.body.appendChild(a)
    a.click()
    document.body.removeChild(a)
    URL.revokeObjectURL(url)
    setDownloaded(true)
  }

  const tooltip =
    `After download, move it into place:\n` +
    `  mv ~/Downloads/${keyName} ~/.ssh/${keyName}\n` +
    `  chmod 600 ~/.ssh/${keyName}`

  return (
    <div className="flex items-center gap-3 flex-wrap">
      <button
        type="button"
        onClick={download}
        title={tooltip}
        className="inline-flex items-center gap-2 px-4 py-2.5 rounded-[10px] bg-ink text-white font-mono text-xs tracking-wide hover:bg-ink-2 transition-colors"
      >
        <span aria-hidden>↓</span>
        <span>DOWNLOAD PRIVATE KEY</span>
      </button>
      <div className="font-mono text-xs text-ink-3">
        <span className="text-ink-2">{keyName}</span>
        {downloaded && <span className="ml-2 text-good">✓ downloaded</span>}
      </div>
    </div>
  )
}

interface ErrorViewProps {
  error: string
  failedStep?: ProvisionStep
  onRetry: () => void
}

function ErrorView({ error, failedStep, onRetry }: ErrorViewProps) {
  const failedLabel = failedStep
    ? PROVISION_PHASES.find((p) => p.step === failedStep)?.label
    : undefined
  return (
    <div className="grid place-items-center min-h-[calc(100vh-160px)]">
      <Card className="w-full max-w-[520px] p-11 text-center">
        <div className="eyebrow text-bad">Provision failed</div>
        <h2 className="text-3xl mt-1">
          {failedLabel ? `Failed during "${failedLabel}".` : 'Something went wrong.'}
        </h2>
        <pre className="mt-6 p-4 rounded-[10px] bg-[rgba(184,58,58,0.06)] text-bad text-xs text-left whitespace-pre-wrap break-words">
          {error}
        </pre>
        <p className="text-sm text-ink-2 mt-4">
          The IP reservation has been released. Check the server logs for details, then
          try again.
        </p>
        <div className="flex justify-center gap-3 mt-6">
          <Button variant="ghost" onClick={onRetry}>
            Back to form
          </Button>
        </div>
      </Card>
    </div>
  )
}

// WorkloadPicker — four radio cards (web/database/compute/balanced) with
// tier-default tracking. When form.workload is null the recommended
// option is highlighted automatically; once the operator picks
// explicitly the choice sticks across tier changes.
//
// Each card's tooltip explains the bias so an operator unfamiliar with
// the labels gets a one-line hint without leaving the page.
function WorkloadPicker({
  tier,
  workload,
  onChange,
}: {
  tier: TierName
  workload: WorkloadType | null
  onChange: (w: WorkloadType | null) => void
}) {
  const recommended = defaultWorkloadForTier(tier)
  const effective = workload ?? recommended
  const opts: { id: WorkloadType; label: string; tip: string }[] = [
    { id: 'web', label: 'Web', tip: 'Web servers, API gateways. Prefers CPU-optimized nodes.' },
    { id: 'database', label: 'Database', tip: 'Databases, caches, in-memory analytics. Prefers memory-optimized nodes.' },
    { id: 'compute', label: 'Compute', tip: 'Builds, training, ML inference. Strongly prefers CPU-optimized nodes.' },
    { id: 'balanced', label: 'Balanced', tip: 'General-purpose workloads. No node-shape preference.' },
  ]
  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-baseline justify-between">
        <label className="text-[13px] font-medium text-ink">Workload type</label>
        {workload !== null && (
          <button
            type="button"
            className="text-[11px] text-ink-3 hover:text-ink underline"
            onClick={() => onChange(null)}
          >
            reset to recommended
          </button>
        )}
      </div>
      <div className="grid grid-cols-2 lg:grid-cols-4 gap-2.5">
        {opts.map((opt) => {
          const selected = effective === opt.id
          const isRecommended = opt.id === recommended
          return (
            <button
              key={opt.id}
              type="button"
              onClick={() => onChange(opt.id)}
              title={opt.tip}
              className={`relative text-left px-3.5 py-3 rounded-[10px] border transition-colors cursor-pointer ${
                selected
                  ? 'border-ink bg-[rgba(27,23,38,0.05)]'
                  : 'border-line-2 hover:border-ink-3'
              }`}
            >
              <div className="flex items-center justify-between gap-2">
                <span className="font-medium text-[13px] text-ink">{opt.label}</span>
                {isRecommended && workload === null && (
                  <span className="font-mono text-[9px] uppercase tracking-widest text-ink-3">
                    Auto
                  </span>
                )}
              </div>
              <p className="text-[11px] text-ink-3 mt-1 leading-relaxed">{opt.tip}</p>
            </button>
          )
        })}
      </div>
      <p className="text-xs text-ink-3 mt-0.5 leading-relaxed">
        Drives node placement. Larger tiers default to{' '}
        <code className="text-ink-2">{recommended}</code> — override if your VM doesn't fit
        that profile.
      </p>
    </div>
  )
}
