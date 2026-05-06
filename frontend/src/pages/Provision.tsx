import { useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import {
  provisionVMStreaming,
  ProvisionError,
  getBootstrapStatus,
  bootstrapTemplates,
  getSDNStatus,
  listKeys,
  listNodes,
  listSubnets,
  getTunnelInfo,
  getGPUInference,
  type BootstrapResult,
  type PublicSDNStatus,
  type Subnet,
  type TunnelInfo,
} from '@/api/client'
import type { GPUInferenceStatus } from '@/types'
import { useAuth } from '@/hooks/useAuth'
import Card from '@/components/ui/Card'
import Button from '@/components/ui/Button'
import Input from '@/components/ui/Input'
import Textarea from '@/components/ui/Textarea'
import TierCard from '@/components/ui/TierCard'
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
} from '@/types'

type ViewState = 'form' | 'loading' | 'result' | 'error'
type KeyMode = 'saved' | 'byo' | 'gen'

const TIER_ORDER: TierName[] = ['small', 'medium', 'large', 'xl']

interface FormState {
  hostname: string
  tier: TierName
  // requiredTags is the host-aggregate constraint as a CSV string
  // (e.g. "fast-cpu,nvme"). Empty = no constraint. Free-form text;
  // the cluster's existing operator-defined tags surface as quick-pick
  // chips in the AffinityPicker below.
  //
  // Defaults to "x86" because every cloud-init template Nimbus ships
  // (ubuntu-22.04 / ubuntu-24.04 / debian-11 / debian-12) is x86_64 —
  // KVM can't cross-arch, so landing one of those images on an ARM
  // host fails at boot. Operators with ARM hardware + ARM templates
  // remove the chip and add `arm` instead.
  requiredTags: string
  os: OSTemplate
  keyMode: KeyMode
  savedKeyId: number | null
  pubKey: string
  privKey: string
  publicTunnel: boolean
  enableGPU: boolean
  // Network attachment. Reflects the four cases the picker can
  // render:
  //   - 'default'  → user's default subnet (auto-create on first provision)
  //   - 'existing' → pick a saved subnet by ID
  //   - 'new'      → create a new subnet inline with this name
  //   - 'bridge'   → admin-only escape hatch; attach to vmbr0 directly
  // The picker hides modes the user can't reach: SDN-off shows only
  // 'bridge' (no choice); SDN-on shows subnet modes plus 'bridge' for
  // admins only.
  subnetMode:    'default' | 'existing' | 'new' | 'bridge'
  savedSubnetId: number | null
  newSubnetName: string
}

const DEFAULT_FORM: FormState = {
  hostname: '',
  tier: 'medium',
  requiredTags: 'x86',
  os: 'ubuntu-24.04',
  keyMode: 'gen',
  savedKeyId: null,
  pubKey: '',
  privKey: '',
  publicTunnel: false,
  enableGPU: false,
  subnetMode:    'default',
  savedSubnetId: null,
  newSubnetName: '',
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

  // Saved subnets. Same shape as savedKeys: lazy-load, leave empty if
  // SDN is disabled cluster-wide (the API returns []) — the form
  // defaults to subnetMode='default' which the backend resolves to
  // either the user's actual default or a fresh auto-create.
  const [savedSubnets, setSavedSubnets] = useState<Subnet[]>([])

  // Public SDN status. Drives the picker's mode set: when off, only
  // 'bridge' renders (greyed, single-option); when on, members get
  // subnet modes only, admins get subnet + bridge escape hatch.
  const [sdnStatus, setSdnStatus] = useState<PublicSDNStatus | null>(null)
  useEffect(() => {
    getSDNStatus()
      .then((s) => {
        setSdnStatus(s)
        // When SDN is off cluster-wide, the only meaningful option is
        // bridge — flip the form into that mode so the picker doesn't
        // show "Default" pre-selected against a meaningless backend.
        if (!s.enabled) {
          setForm((prev) => ({ ...prev, subnetMode: 'bridge' }))
        }
      })
      .catch(() => setSdnStatus({ enabled: false, default_bridge: 'vmbr0' }))
  }, [])

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
    listSubnets()
      .then((rows) => {
        setSavedSubnets(rows)
        // Pre-select the user's default subnet so the picker UI
        // shows it immediately when they switch to "existing" mode.
        const def = rows.find((s) => s.is_default) ?? rows[0]
        if (def) {
          setForm((prev) =>
            prev.savedSubnetId === null
              ? { ...prev, savedSubnetId: def.id }
              : prev,
          )
        }
      })
      .catch(() => {
        // Non-fatal — SDN may be disabled cluster-wide; form still
        // works with subnetMode='default' which the backend resolves.
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
          // Empty required-tags omitted so the JSON stays clean.
          // Server normalizes empty CSV to "no constraint" either way.
          required_tags: form.requiredTags.trim() || undefined,
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
          subnet_id:
            form.subnetMode === 'existing' && form.savedSubnetId !== null
              ? form.savedSubnetId
              : undefined,
          subnet_name:
            form.subnetMode === 'new' && form.newSubnetName.trim() !== ''
              ? form.newSubnetName.trim()
              : undefined,
          bridge:
            form.subnetMode === 'bridge' && sdnStatus
              ? sdnStatus.default_bridge
              : undefined,
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
              savedSubnets={savedSubnets}
              sdnStatus={sdnStatus}
              isAdmin={user?.is_admin ?? false}
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
  savedSubnets: Subnet[]
  sdnStatus: PublicSDNStatus | null
  isAdmin: boolean
  tunnelInfo: TunnelInfo | null
  gpuInfo: GPUInferenceStatus | null
  // Mirrors the backend rule that public SSH needs a private half — the
  // parent computes it once and passes it down so this component doesn't
  // duplicate the keyMode → has_private_key logic.
  selectedKeyHasPrivate: boolean
}

function FormBody({ form, updateForm, savedKeys, savedSubnets, sdnStatus, isAdmin, tunnelInfo, gpuInfo, selectedKeyHasPrivate }: FormBodyProps) {
  return (
    <div className="flex flex-col gap-5">
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

      <div className="flex flex-col gap-2">
        <label className="text-[13px] font-medium text-ink">SSH key</label>
        <KeyModeButtons
          mode={form.keyMode}
          onChange={(m) => updateForm('keyMode', m)}
          hasSavedKeys={savedKeys.length > 0}
        />
        <p className="text-[11px] text-ink-3 leading-relaxed mt-1">{keyModeBlurb(form.keyMode, savedKeys)}</p>
        {form.keyMode === 'saved' && savedKeys.length > 0 && (
          <select
            value={form.savedKeyId ?? ''}
            onChange={(e) => updateForm('savedKeyId', Number(e.target.value))}
            className="mt-2 w-full px-3.5 py-3 rounded-[10px] bg-white/85 font-sans text-sm text-ink border border-line-2 outline-none focus:border-ink focus:bg-white"
          >
            {savedKeys.map((k) => (
              <option key={k.id} value={k.id}>
                {k.name}
                {k.is_default ? ' · default' : ''}
                {k.label ? ` — ${k.label}` : ''}
              </option>
            ))}
          </select>
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

      <SubnetPicker
        form={form}
        updateForm={updateForm}
        savedSubnets={savedSubnets}
        sdnStatus={sdnStatus}
        isAdmin={isAdmin}
      />

      <div className="flex flex-col gap-2">
        <label className="text-[13px] font-medium text-ink">Public access</label>
        <label
          className={`flex items-start gap-3 p-3 rounded-[10px] border border-line-2 bg-white/85 transition-colors ${
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
              Reverse-tunnel SSH via Gopher to{' '}
              <span className="font-mono">{tunnelInfo?.host || 'gateway'}:&lt;port&gt;</span>.
              Expose web services later from the machine page.
            </div>
            {!tunnelInfo?.enabled && (
              <div className="text-xs text-warn mt-1">
                Tunnel integration not configured —{' '}
                <Link to="/infrastructure/gopher" className="underline">admin setup</Link>.
              </div>
            )}
            {tunnelInfo?.enabled && !selectedKeyHasPrivate && (
              <div className="text-xs text-warn mt-1">
                {form.keyMode === 'byo'
                  ? 'Paste the private half above — Nimbus needs it to bootstrap the tunnel.'
                  : form.keyMode === 'saved'
                    ? <>Selected key has no private half. Pick another or upload it on <Link to="/keys" className="underline">SSH keys</Link>.</>
                    : 'Pick a key with a private half — the bootstrap needs to SSH into the VM.'}
              </div>
            )}
          </div>
        </label>
      </div>

      <AdvancedSection>
        <AffinityPicker
          value={form.requiredTags}
          onChange={(v) => updateForm('requiredTags', v)}
        />
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
      </AdvancedSection>
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
  const isolated = Boolean(result.subnet_name)
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
              <strong>Guest agent did not confirm.</strong> {result.warning}
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

        {result.cloud_init_error && (
          <div className="mt-5 p-4 rounded-[10px] bg-[rgba(184,101,15,0.08)] border border-[rgba(184,101,15,0.2)] text-warn text-[13px] leading-relaxed flex items-start gap-2.5">
            <span className="text-base">⚠</span>
            <div>
              <strong>Cloud-init ISO not delivered.</strong>{' '}
              The per-VM cloud-init ISO (which installs the qemu-guest-agent so
              readiness checks succeed) couldn't be uploaded or attached. Check that
              the configured storage accepts <span className="font-mono text-ink">iso</span>{' '}
              content and that the API token has{' '}
              <span className="font-mono text-ink">Datastore.AllocateTemplate</span>{' '}
              on it. Reason: <span className="font-mono text-ink whitespace-pre-line">{result.cloud_init_error}</span>
            </div>
          </div>
        )}

        {isolated && !hasTunnel && (
          <div className="mt-5 p-4 rounded-[10px] bg-[rgba(45,77,125,0.06)] border border-[rgba(45,77,125,0.2)] text-ink-2 text-[13px] leading-relaxed flex items-start gap-2.5">
            <span className="text-base">🔒</span>
            <div>
              <strong>Isolated subnet.</strong>{' '}
              This VM lives on{' '}
              <span className="font-mono text-ink">{result.subnet_name}</span>
              {result.subnet_cidr && (
                <>
                  {' '}(<span className="font-mono text-ink">{result.subnet_cidr}</span>)
                </>
              )}
              . The IP below isn't reachable from the public internet or the
              cluster's main LAN — it's only reachable from another VM in the
              same subnet, the Proxmox host, or via a tunnel. To connect now:
              open the Proxmox console for VMID{' '}
              <span className="font-mono text-ink">{result.vmid}</span> on node{' '}
              <span className="font-mono text-ink">{result.node}</span>, or
              re-provision with the public-SSH toggle for a Gopher tunnel.
            </div>
          </div>
        )}

        <div className="grid grid-cols-1 sm:grid-cols-2 gap-3 mt-7">
          <CredCell label="Hostname" value={result.hostname} />
          <CredCell label={isolated ? 'Subnet IP' : 'Local IP'} value={result.ip} />
          <CredCell label="Username" value={result.username} />
          {result.console_password && (
            <CredCell
              label="Console password (one-time)"
              value={result.console_password}
            />
          )}
          <CredCell label="VMID / Node" value={`${result.vmid} on ${result.node}`} />
          <CredCell
            label={hasTunnel ? 'SSH (LAN)' : isolated ? 'SSH (from inside subnet)' : 'SSH command'}
            value={sshCommand}
            fullWidth
          />
        </div>
        {result.console_password && (
          <p className="text-xs text-ink-3 mt-2 leading-relaxed">
            🔑 Console password is a one-time fallback for the Proxmox noVNC console
            (VMID {result.vmid} → Console). Save it now — it's not stored anywhere
            and a new one is generated on every provision.
          </p>
        )}

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

// SUGGESTED_TAGS are surfaced as quick-pick chips even when no node in
// the cluster carries them yet. `fast-cpu` is a relative-speed marker
// the operator opts into manually — auto-assigning it would mean
// picking "fastest in this cluster", which is meaningless across
// deployments. Surfacing it here keeps the vocabulary discoverable.
const SUGGESTED_TAGS = ['fast-cpu']

// AffinityPicker — comma-separated tag input for the host-aggregate
// constraint. Operators tag nodes ("fast-cpu", "nvme", "gpu") on the
// /nodes page; the picker offers existing cluster tags as quick-pick
// chips and lets the user free-type custom values too.
//
// Three sources feed the chip row:
//   1. operator-applied tags (db.Node.Tags),
//   2. auto-derived system tags (arch: x86 / arm — currently the only
//      ones; emitted by nodescore.DeriveAutoTags from cpu_model),
//   3. SUGGESTED_TAGS — curated vocabulary that appears even when no
//      node carries the tag yet.
//
// SubnetPicker is the per-VM network attachment chooser. Adapts to
// the cluster's SDN state and the caller's role:
//
//   SDN off (cluster-wide):
//     - Single greyed "Cluster LAN" tile, pre-selected. No choice.
//       Note explains "isolation is off; admin can enable it."
//
//   SDN on, member:
//     - Default / Existing / + New subnet (three-chip picker).
//       No bridge option — isolation is enforced.
//
//   SDN on, admin:
//     - Default / Existing / + New subnet PLUS a "Cluster LAN
//       (admin)" escape-hatch chip. Used for management VMs that
//       need to reach the cluster LAN directly.
//
// The state field `subnetMode` is the source of truth; modes the
// caller can't reach are simply not rendered. The form initializer
// flips `subnetMode` to 'bridge' on first load when SDN is off so
// the submit payload sends `bridge=vmbr0` rather than meaningless
// subnet fields.
function SubnetPicker({
  form,
  updateForm,
  savedSubnets,
  sdnStatus,
  isAdmin,
}: {
  form: FormState
  updateForm: <K extends keyof FormState>(key: K, value: FormState[K]) => void
  savedSubnets: Subnet[]
  sdnStatus: PublicSDNStatus | null
  isAdmin: boolean
}) {
  // Status hasn't loaded yet — render a placeholder rather than
  // flashing the wrong picker shape and rebinding the form mode.
  if (!sdnStatus) {
    return (
      <div className="flex flex-col gap-2">
        <label className="text-[13px] font-medium text-ink">Network</label>
        <p className="text-[11px] text-ink-3">Loading…</p>
      </div>
    )
  }

  // SDN-off branch: the picker is informational, not interactive.
  if (!sdnStatus.enabled) {
    return (
      <div className="flex flex-col gap-2">
        <label className="text-[13px] font-medium text-ink">Network</label>
        <ModeChip active disabled={false} onClick={() => undefined}>
          Cluster LAN ({sdnStatus.default_bridge})
        </ModeChip>
        <p className="text-[11px] text-ink-3">
          Per-user isolation is off — admins can enable it on{' '}
          <Link to="/infrastructure/network" className="underline">VM network</Link>.
        </p>
      </div>
    )
  }

  // SDN-on branch: subnet picker for everyone, bridge escape hatch
  // for admins only.
  const hasSaved = savedSubnets.length > 0
  return (
    <div className="flex flex-col gap-2">
      <label className="text-[13px] font-medium text-ink">Subnet</label>
      <div className="flex flex-wrap gap-2">
        <ModeChip
          active={form.subnetMode === 'default'}
          onClick={() => updateForm('subnetMode', 'default')}
        >
          Default {hasSaved && (() => {
            const def = savedSubnets.find((s) => s.is_default)
            return def ? <span className="text-ink-3">({def.name})</span> : null
          })()}
        </ModeChip>
        <ModeChip
          active={form.subnetMode === 'existing'}
          disabled={!hasSaved}
          onClick={() => updateForm('subnetMode', 'existing')}
        >
          Existing
        </ModeChip>
        {isAdmin && (
          <ModeChip
            active={form.subnetMode === 'bridge'}
            onClick={() => updateForm('subnetMode', 'bridge')}
          >
            Cluster LAN <span className="text-ink-3">(admin)</span>
          </ModeChip>
        )}
        <ModeChip
          active={form.subnetMode === 'new'}
          onClick={() => updateForm('subnetMode', 'new')}
        >
          + New subnet
        </ModeChip>
      </div>
      {form.subnetMode === 'existing' && hasSaved && (
        <select
          value={form.savedSubnetId ?? ''}
          onChange={(e) =>
            updateForm('savedSubnetId', e.target.value === '' ? null : Number(e.target.value))
          }
          className="n-input"
        >
          {savedSubnets.map((s) => (
            <option key={s.id} value={s.id}>
              {s.name} ({s.subnet}){s.is_default ? ' · default' : ''}
            </option>
          ))}
        </select>
      )}
      {form.subnetMode === 'new' && (
        <Input
          value={form.newSubnetName}
          onChange={(e) => updateForm('newSubnetName', e.target.value)}
          placeholder="web-tier"
          maxLength={32}
        />
      )}
      {form.subnetMode === 'bridge' && (
        <p className="text-[11px] text-warn leading-relaxed">
          ⚠ Admin-only. VM lands on{' '}
          <span className="font-mono">{sdnStatus.default_bridge}</span>,
          bypassing isolation.
        </p>
      )}
      <p className="text-[11px] text-ink-3">
        Subnets isolate VMs from the cluster LAN and each other —{' '}
        <Link to="/subnets" className="underline">manage</Link>.
      </p>
    </div>
  )
}

function ModeChip({
  active,
  disabled,
  onClick,
  children,
}: {
  active: boolean
  disabled?: boolean
  onClick: () => void
  children: React.ReactNode
}) {
  const base =
    'px-3 py-1.5 rounded-[8px] text-[12px] font-medium border cursor-pointer transition-colors'
  const cls = disabled
    ? `${base} border-line text-ink-3 cursor-not-allowed opacity-50`
    : active
      ? `${base} bg-ink text-white border-ink`
      : `${base} bg-white/85 border-line-2 text-ink-2 hover:border-ink/40`
  return (
    <button type="button" disabled={disabled} onClick={onClick} className={cls}>
      {children}
    </button>
  )
}

// Empty value = no constraint (capacity-based scoring only). Multiple
// tags AND together — every required tag must be on the destination
// node or it's filtered out.
function AffinityPicker({
  value,
  onChange,
}: {
  value: string
  onChange: (v: string) => void
}) {
  const [knownTags, setKnownTags] = useState<string[]>([])
  // Lazy-load the cluster's tag inventory once on mount. The /nodes
  // payload already carries each node's tags; we union them client-
  // side rather than adding a dedicated endpoint.
  useEffect(() => {
    listNodes()
      .then((rows) => {
        const set = new Set<string>(SUGGESTED_TAGS)
        for (const n of rows) {
          for (const t of n.tags || []) set.add(t)
          for (const t of n.auto_tags || []) set.add(t)
        }
        setKnownTags(Array.from(set).sort())
      })
      .catch(() => {
        // Non-fatal — fall back to just the curated suggestions.
        setKnownTags([...SUGGESTED_TAGS].sort())
      })
  }, [])

  const selected = new Set(value.split(',').map((t) => t.trim()).filter(Boolean))
  const toggle = (tag: string) => {
    const next = new Set(selected)
    if (next.has(tag)) next.delete(tag)
    else next.add(tag)
    onChange(Array.from(next).join(', '))
  }

  return (
    <div className="flex flex-col gap-2">
      <label className="text-[13px] font-medium text-ink">Required hardware tags</label>
      <input
        type="text"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder="e.g. fast-cpu, nvme"
        className="w-full px-3.5 py-2.5 rounded-[10px] bg-white/85 font-mono text-sm text-ink border border-line-2 outline-none focus:border-ink focus:bg-white"
      />
      {knownTags.length > 0 && (
        <div className="flex flex-wrap items-center gap-1.5 mt-0.5">
          <span className="text-[11px] text-ink-3 font-mono">cluster tags:</span>
          {knownTags.map((tag) => {
            const isSel = selected.has(tag)
            return (
              <button
                key={tag}
                type="button"
                onClick={() => toggle(tag)}
                className={`font-mono text-[11px] px-2 py-0.5 rounded-[5px] border transition-colors cursor-pointer ${
                  isSel
                    ? 'bg-ink text-white border-ink'
                    : 'bg-transparent border-line-2 text-ink-2 hover:border-ink-3 hover:text-ink'
                }`}
              >
                {tag}
              </button>
            )
          })}
        </div>
      )}
      <p className="text-xs text-ink-3 mt-0.5 leading-relaxed">
        Comma-separated. The scheduler only places this VM on nodes carrying every listed tag —
        operators apply tags from <a href="/nodes" className="underline">/nodes</a>. Defaults to
        <code className="font-mono mx-1">x86</code> because Nimbus's cloud-init templates are all
        x86_64; remove it (and add <code className="font-mono mx-1">arm</code>) only when targeting
        ARM hosts with an ARM-built template. Empty = no constraint.
      </p>
    </div>
  )
}

// KeyModeButtons — horizontal segmented control replacing the previous
// stack of three RadioCards. Two buttons when there are no saved keys
// (Generate / Bring your own); three when at least one saved key
// exists (Saved / Generate / BYO).
//
// Each button is equal-width via flex-1; the selected one inverts to
// dark fill, matching the tier-pill convention elsewhere in the app.
// The longer description that used to live on each card moves to a
// single-line caption underneath the row that updates per selection
// (see keyModeBlurb).
function KeyModeButtons({
  mode,
  onChange,
  hasSavedKeys,
}: {
  mode: KeyMode
  onChange: (m: KeyMode) => void
  hasSavedKeys: boolean
}) {
  const opts: { id: KeyMode; label: string }[] = []
  if (hasSavedKeys) opts.push({ id: 'saved', label: 'Use a saved key' })
  opts.push({ id: 'gen', label: 'Generate one for me' })
  opts.push({ id: 'byo', label: 'Bring your own' })
  return (
    <div className="flex gap-1.5">
      {opts.map((opt) => {
        const selected = mode === opt.id
        return (
          <button
            key={opt.id}
            type="button"
            onClick={() => onChange(opt.id)}
            className={`flex-1 px-3 py-2.5 rounded-[8px] text-[13px] font-medium border transition-colors cursor-pointer ${
              selected
                ? 'bg-ink text-white border-ink'
                : 'bg-white/85 text-ink border-line-2 hover:border-ink-3'
            }`}
          >
            {opt.label}
          </button>
        )
      })}
    </div>
  )
}

// keyModeBlurb — one-line caption beneath the segmented control. The
// longer descriptions that used to live on each RadioCard surface here
// based on the current selection. Kept short so the form stays dense.
function keyModeBlurb(mode: KeyMode, savedKeys: SSHKey[]): string {
  switch (mode) {
    case 'saved':
      return savedKeys.find((k) => k.is_default)
        ? 'Your default key is selected — pick a different one if needed.'
        : "Pick from the keys you've already added."
    case 'gen':
      return "We'll mint an Ed25519 keypair, vault it, and show the private key once."
    case 'byo':
      return 'Paste or upload a public key. Optionally store the private half so you can download it later.'
  }
}

// AdvancedSection — collapsible disclosure for less-frequently-touched
// settings. Used to wrap hardware tags + GPU access at the bottom of
// the form so the default view stays focused on the core identity
// (hostname / OS / tier / SSH / public access). Collapsed by default;
// chevron rotates on open.
function AdvancedSection({ children }: { children: React.ReactNode }) {
  const [open, setOpen] = useState(false)
  return (
    <div className="flex flex-col gap-3">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        className="flex items-center gap-2 text-[13px] font-medium text-ink-2 hover:text-ink transition-colors cursor-pointer self-start"
        aria-expanded={open}
      >
        <span
          aria-hidden="true"
          className="text-ink-3 transition-transform"
          style={{ transform: open ? 'rotate(90deg)' : 'rotate(0deg)' }}
        >▶</span>
        Advanced
        <span className="text-[11px] text-ink-3 font-normal">hardware tags, GPU access</span>
      </button>
      {open && (
        <div className="flex flex-col gap-5 pl-1">
          {children}
        </div>
      )}
    </div>
  )
}
