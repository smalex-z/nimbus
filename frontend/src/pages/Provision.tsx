import { useEffect, useMemo, useState } from 'react'
import {
  provisionVM,
  getBootstrapStatus,
  bootstrapTemplates,
  type BootstrapResult,
} from '@/api/client'
import Card from '@/components/ui/Card'
import Button from '@/components/ui/Button'
import Input from '@/components/ui/Input'
import Textarea from '@/components/ui/Textarea'
import TierCard from '@/components/ui/TierCard'
import RadioCard from '@/components/ui/RadioCard'
import CopyButton from '@/components/ui/CopyButton'
import {
  OS_OPTIONS,
  type OSTemplate,
  type ProvisionResult,
  TIERS,
  type TierName,
} from '@/types'

type ViewState = 'form' | 'loading' | 'result' | 'error'
type KeyMode = 'byo' | 'gen'

const TIER_ORDER: TierName[] = ['small', 'medium', 'large', 'xl']

interface FormState {
  hostname: string
  tier: TierName
  os: OSTemplate
  keyMode: KeyMode
  pubKey: string
}

const DEFAULT_FORM: FormState = {
  hostname: '',
  tier: 'medium',
  os: 'ubuntu-24.04',
  keyMode: 'byo',
  pubKey: '',
}

export default function Provision() {
  const [view, setView] = useState<ViewState>('form')
  const [form, setForm] = useState<FormState>(DEFAULT_FORM)
  const [result, setResult] = useState<ProvisionResult | null>(null)
  const [error, setError] = useState<string | null>(null)

  const [bootstrapped, setBootstrapped] = useState<boolean | null>(null)
  const [bootstrapRunning, setBootstrapRunning] = useState(false)
  const [bootstrapResult, setBootstrapResult] = useState<BootstrapResult | null>(null)
  const [bootstrapError, setBootstrapError] = useState<string | null>(null)
  const [bootstrapElapsed, setBootstrapElapsed] = useState(0)

  useEffect(() => {
    getBootstrapStatus()
      .then((s) => setBootstrapped(s.bootstrapped))
      .catch(() => setBootstrapped(false))
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

  const canSubmit = useMemo(() => {
    if (!form.hostname || form.hostname.length === 0) return false
    if (form.tier === 'xl') return false
    if (form.keyMode === 'byo' && form.pubKey.trim().length === 0) return false
    return true
  }, [form])

  const updateForm = <K extends keyof FormState>(key: K, value: FormState[K]) =>
    setForm((prev) => ({ ...prev, [key]: value }))

  const reset = () => {
    setForm(DEFAULT_FORM)
    setResult(null)
    setError(null)
    setView('form')
  }

  const submit = async () => {
    setError(null)
    setView('loading')
    try {
      const res = await provisionVM({
        hostname: form.hostname,
        tier: form.tier,
        os_template: form.os,
        ssh_pubkey: form.keyMode === 'byo' ? form.pubKey.trim() : undefined,
        generate_key: form.keyMode === 'gen' ? true : undefined,
      })
      setResult(res)
      setView('result')
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : 'unknown error'
      setError(msg)
      setView('error')
    }
  }

  if (view === 'loading') {
    return <LoadingView hostname={form.hostname} />
  }
  if (view === 'result' && result) {
    return <ResultView result={result} onReset={reset} />
  }
  if (view === 'error') {
    return <ErrorView error={error ?? 'unknown'} onRetry={() => setView('form')} />
  }

  return (
    <div>
      <div className="flex items-end justify-between flex-wrap gap-4 mb-2">
        <div>
          <div className="eyebrow">New machine</div>
          <h2 className="text-3xl">What are we spinning up today?</h2>
          <p className="text-base text-ink-2 mt-2 leading-relaxed">
            Pick a size, paste a key, give it a name. Done in &lt; 60s.
          </p>
        </div>
      </div>

      {bootstrapped === null && (
        <div className="mt-12 grid place-items-center">
          <div className="w-4 h-4 border-[1.5px] border-ink-3 border-t-ink rounded-full animate-spin" />
        </div>
      )}

      {bootstrapped === false && (
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
            <FormBody form={form} updateForm={updateForm} />
          </Card>

          <Card className="p-7 self-start lg:sticky lg:top-[100px]">
            <Summary
              form={form}
              tierLabel={`${selectedTier.cpu} / ${selectedTier.memMB / 1024} GB`}
              diskLabel={`${selectedTier.diskGB} GB`}
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
              Typically completes in 30–60 seconds.
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
          This one-time download (~2 GB per OS) runs in parallel across all cluster nodes
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

// ── Form ──────────────────────────────────────────────────────────────────────

interface FormBodyProps {
  form: FormState
  updateForm: <K extends keyof FormState>(key: K, value: FormState[K]) => void
}

function FormBody({ form, updateForm }: FormBodyProps) {
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

      <div className="flex flex-col gap-2">
        <label className="text-[13px] font-medium text-ink">SSH key</label>
        <div className="grid gap-2">
          <RadioCard
            title="Bring your own key"
            description="Paste a public key. We never store the private half."
            selected={form.keyMode === 'byo'}
            onClick={() => updateForm('keyMode', 'byo')}
          />
          <RadioCard
            title="Generate one for me"
            description="We'll mint an Ed25519 keypair and show you the private key once."
            selected={form.keyMode === 'gen'}
            onClick={() => updateForm('keyMode', 'gen')}
          />
        </div>
        {form.keyMode === 'byo' && (
          <div className="mt-4">
            <Textarea
              monospace
              placeholder="ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI... you@laptop"
              value={form.pubKey}
              onChange={(e) => updateForm('pubKey', e.target.value)}
            />
          </div>
        )}
      </div>

      <div className="flex flex-col gap-2 opacity-50">
        <label className="text-[13px] font-medium text-ink">Public access</label>
        <div className="flex items-center gap-3 p-3.5 rounded-[10px] border border-line-2 bg-white/60">
          <span className="w-[18px] h-[18px] rounded-md border-[1.5px] border-ink-3 bg-white" />
          <div className="flex-1">
            <div className="text-sm font-medium">Expose at a public hostname</div>
            <div className="text-xs text-ink-3 mt-0.5">
              Coming in Phase 2 — Gopher tunnel integration.
            </div>
          </div>
        </div>
      </div>
    </div>
  )
}

interface SummaryProps {
  form: FormState
  tierLabel: string
  diskLabel: string
}

function Summary({ form, tierLabel, diskLabel }: SummaryProps) {
  return (
    <>
      <h3 className="text-xl font-semibold">Summary</h3>
      <div className="mt-4">
        <SummaryRow label="Hostname" value={form.hostname || '—'} />
        <SummaryRow label="OS" value={form.os} />
        <SummaryRow label="Tier" value={form.tier} />
        <SummaryRow label="vCPU / RAM" value={tierLabel} />
        <SummaryRow label="Disk" value={diskLabel} />
        <SummaryRow
          label="SSH"
          value={form.keyMode === 'byo' ? 'your key' : 'generate'}
        />
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
}

function LoadingView({ hostname }: LoadingViewProps) {
  // Steps mirror the design-doc §5.2 flow. They animate based on elapsed time
  // since this is a synchronous backend call — we don't have a real progress
  // stream. The user sees forward motion which beats a single spinner.
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
          Hold tight — your machine is on its way. Provisioning typically takes 30–60s.
        </p>

        <div className="mt-9 text-left border-t border-line">
          <ProvisionStep label="Validating request & allocating IP" status="active" />
          <ProvisionStep label="Cloning golden template" status="pending" />
          <ProvisionStep label="Applying cloud-init configuration" status="pending" />
          <ProvisionStep label="Booting & waiting for guest agent" status="pending" />
        </div>
      </Card>
    </div>
  )
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
        {status === 'done' && <span className="text-white text-[10px] font-bold">✓</span>}
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
  const sshCommand = `ssh ${result.username}@${result.ip}`
  const hasWarning = Boolean(result.warning)
  const statusLabel = hasWarning ? 'MACHINE READY (UNVERIFIED)' : 'MACHINE READY'
  const statusColorClass = hasWarning
    ? 'bg-[rgba(184,101,15,0.12)] text-warn'
    : 'bg-[rgba(45,125,90,0.1)] text-good'
  const dotColorClass = hasWarning ? 'bg-warn' : 'bg-good'
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

        <div className="grid grid-cols-1 sm:grid-cols-2 gap-3 mt-7">
          <CredCell label="Hostname" value={result.hostname} />
          <CredCell label="IP address" value={result.ip} />
          <CredCell label="Username" value={result.username} />
          <CredCell label="VMID / Node" value={`${result.vmid} on ${result.node}`} />
          <CredCell label="SSH command" value={sshCommand} fullWidth />
        </div>

        {result.ssh_private_key && (
          <div className="mt-6">
            <div className="text-[10px] font-mono uppercase tracking-widest text-ink-3 mb-1.5">
              Private key (Ed25519)
            </div>
            <pre className="p-4 rounded-[10px] bg-ink text-white font-mono text-[11px] leading-relaxed overflow-auto max-h-[200px] whitespace-pre">
              {result.ssh_private_key}
            </pre>
            <CopyButton
              value={result.ssh_private_key}
              label="COPY KEY"
              className="mt-2 px-3 py-1.5"
            />

            <div className="mt-6 p-3.5 rounded-[10px] bg-[rgba(184,101,15,0.08)] border border-[rgba(184,101,15,0.2)] text-warn text-[13px] leading-relaxed flex items-start gap-2.5">
              <span className="text-base">⚠</span>
              <div>
                <strong>This is the only time you'll see the private key.</strong>{' '}
                Save it to your SSH config now. Nimbus does not store it anywhere.
              </div>
            </div>
          </div>
        )}

        <div className="flex gap-2.5 justify-end mt-9">
          <Button variant="ghost" onClick={onReset}>
            Provision another
          </Button>
        </div>
      </Card>
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

interface ErrorViewProps {
  error: string
  onRetry: () => void
}

function ErrorView({ error, onRetry }: ErrorViewProps) {
  return (
    <div className="grid place-items-center min-h-[calc(100vh-160px)]">
      <Card className="w-full max-w-[520px] p-11 text-center">
        <div className="eyebrow text-bad">Provision failed</div>
        <h2 className="text-3xl mt-1">Something went wrong.</h2>
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
