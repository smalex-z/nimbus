package handlers

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"nimbus/internal/api/response"
	"nimbus/internal/ctxutil"
	"nimbus/internal/db"
	"nimbus/internal/gpu"
	"nimbus/internal/service"
)

// GPU wraps the gpu.Service plus the AuthService for settings access.
type GPU struct {
	svc  *gpu.Service
	auth *service.AuthService
	// nimbusBaseURL is the base URL of THIS Nimbus instance (e.g.
	// https://nimbus.example.com) — embedded into the install bootstrap
	// script so the GX10's worker knows where to phone home. Pulled from
	// AppURL at construction.
	nimbusBaseURL string
	// gpuConfigApplier is invoked after a successful pairing register so
	// the live provision.Service picks up the new base_url + model without
	// a Nimbus restart. The third arg is a fully-resolved nimbusGPUAPI
	// URL (e.g. https://nimbus.example.com/api/gpu) — Register derives it
	// from the request host (proven reachable by the GX10) so a misconfigured
	// APP_URL doesn't poison the bootstrap config. Pass "" to fall back
	// to whatever default the applier wires up (used by Unpair).
	// Nil applier is allowed.
	gpuConfigApplier func(baseURL, model, nimbusGPUAPI string)
}

// NewGPU wires the handler.
func NewGPU(svc *gpu.Service, auth *service.AuthService, nimbusBaseURL string) *GPU {
	return &GPU{svc: svc, auth: auth, nimbusBaseURL: strings.TrimRight(nimbusBaseURL, "/")}
}

// WithGPUConfigApplier installs the post-register callback. Builder-style so
// router wiring stays a single fluent expression.
func (h *GPU) WithGPUConfigApplier(fn func(baseURL, model, nimbusGPUAPI string)) *GPU {
	h.gpuConfigApplier = fn
	return h
}

// gpuJobView is the JSON projection of db.GPUJob with timestamps formatted
// as RFC3339 and the env decoded back to map for the frontend.
type gpuJobView struct {
	ID           uint              `json:"id"`
	OwnerID      uint              `json:"owner_id"`
	VMID         *uint             `json:"vm_id,omitempty"`
	Status       string            `json:"status"`
	Image        string            `json:"image"`
	Command      string            `json:"command"`
	Env          map[string]string `json:"env,omitempty"`
	WorkerID     string            `json:"worker_id,omitempty"`
	ExitCode     *int              `json:"exit_code,omitempty"`
	ArtifactPath string            `json:"artifact_path,omitempty"`
	ErrorMsg     string            `json:"error_msg,omitempty"`
	QueuedAt     string            `json:"queued_at"`
	StartedAt    string            `json:"started_at,omitempty"`
	FinishedAt   string            `json:"finished_at,omitempty"`
	LogTail      string            `json:"log_tail,omitempty"`
}

func toGPUJobView(j *db.GPUJob) gpuJobView {
	v := gpuJobView{
		ID:           j.ID,
		OwnerID:      j.OwnerID,
		VMID:         j.VMID,
		Status:       j.Status,
		Image:        j.Image,
		Command:      j.Command,
		WorkerID:     j.WorkerID,
		ExitCode:     j.ExitCode,
		ArtifactPath: j.ArtifactPath,
		ErrorMsg:     j.ErrorMsg,
		QueuedAt:     j.QueuedAt.Format(time.RFC3339),
		LogTail:      j.LogTail,
	}
	if j.StartedAt != nil {
		v.StartedAt = j.StartedAt.Format(time.RFC3339)
	}
	if j.FinishedAt != nil {
		v.FinishedAt = j.FinishedAt.Format(time.RFC3339)
	}
	if j.EnvJSON != "" {
		_ = json.Unmarshal([]byte(j.EnvJSON), &v.Env) // tolerate corruption — empty env is fine
	}
	return v
}

// ----------------- user-facing routes -----------------

type submitJobRequest struct {
	Image   string            `json:"image"`
	Command string            `json:"command"`
	Env     map[string]string `json:"env,omitempty"`
	VMID    *uint             `json:"vm_id,omitempty"`
}

// SubmitJob handles POST /api/gpu/jobs.
func (h *GPU) SubmitJob(w http.ResponseWriter, r *http.Request) {
	user := ctxutil.User(r.Context())
	if user == nil {
		response.Error(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	settings, err := h.auth.GetGPUSettings()
	if err != nil {
		response.InternalError(w, "failed to load gpu settings")
		return
	}
	if !settings.Enabled || settings.BaseURL == "" {
		response.Error(w, http.StatusServiceUnavailable, "GPU plane is not configured")
		return
	}

	var req submitJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON body")
		return
	}
	job, err := h.svc.EnqueueJob(r.Context(), gpu.EnqueueRequest{
		OwnerID:          user.ID,
		VMID:             req.VMID,
		Image:            req.Image,
		Command:          req.Command,
		Env:              req.Env,
		RequesterIsAdmin: user.IsAdmin,
	})
	if err != nil {
		response.FromError(w, err)
		return
	}
	response.Created(w, toGPUJobView(job))
}

// ListJobs handles GET /api/gpu/jobs. Admins see every job; non-admins see
// their own only. Optional `?status=running` narrows.
func (h *GPU) ListJobs(w http.ResponseWriter, r *http.Request) {
	user := ctxutil.User(r.Context())
	if user == nil {
		response.Error(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	jobs, err := h.svc.ListJobs(r.Context(), gpu.ListFilter{
		OwnerID:          user.ID,
		IncludeAllOwners: user.IsAdmin,
		Status:           r.URL.Query().Get("status"),
	})
	if err != nil {
		response.FromError(w, err)
		return
	}
	out := make([]gpuJobView, 0, len(jobs))
	for i := range jobs {
		out = append(out, toGPUJobView(&jobs[i]))
	}
	response.Success(w, out)
}

// GetJob handles GET /api/gpu/jobs/{id}.
func (h *GPU) GetJob(w http.ResponseWriter, r *http.Request) {
	user := ctxutil.User(r.Context())
	if user == nil {
		response.Error(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	id, err := parseUintParam(r, "id")
	if err != nil {
		response.BadRequest(w, "invalid id")
		return
	}
	job, err := h.svc.GetJob(r.Context(), id, user.ID, user.IsAdmin)
	if err != nil {
		response.FromError(w, err)
		return
	}
	response.Success(w, toGPUJobView(job))
}

// CancelJob handles POST /api/gpu/jobs/{id}/cancel.
func (h *GPU) CancelJob(w http.ResponseWriter, r *http.Request) {
	user := ctxutil.User(r.Context())
	if user == nil {
		response.Error(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	id, err := parseUintParam(r, "id")
	if err != nil {
		response.BadRequest(w, "invalid id")
		return
	}
	job, err := h.svc.CancelJob(r.Context(), id, user.ID, user.IsAdmin)
	if err != nil {
		response.FromError(w, err)
		return
	}
	response.Success(w, toGPUJobView(job))
}

// inferenceView is GET /api/gpu/inference's response.
type inferenceView struct {
	Enabled bool   `json:"enabled"`
	BaseURL string `json:"base_url,omitempty"`
	Model   string `json:"model,omitempty"`
	Status  string `json:"status"` // up | down | unconfigured
}

// Inference handles GET /api/gpu/inference. Includes a best-effort health
// probe of the configured base URL — short timeout so a flaky GX10 doesn't
// stall the page.
func (h *GPU) Inference(w http.ResponseWriter, r *http.Request) {
	settings, err := h.auth.GetGPUSettings()
	if err != nil {
		response.InternalError(w, "failed to load gpu settings")
		return
	}
	v := inferenceView{
		Enabled: settings.Enabled,
		BaseURL: settings.BaseURL,
		Model:   settings.InferenceModel,
		Status:  "unconfigured",
	}
	if settings.Enabled && settings.BaseURL != "" {
		if probeInferenceUp(r.Context(), settings.BaseURL) {
			v.Status = "up"
		} else {
			v.Status = "down"
		}
	}
	response.Success(w, v)
}

// probeInferenceUp does a quick HEAD/GET to the OpenAI-compatible /v1/models
// endpoint. Returns true on any 2xx; everything else (timeout, 5xx) is "down".
func probeInferenceUp(ctx context.Context, baseURL string) bool {
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	target := strings.TrimRight(baseURL, "/") + "/v1/models"
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, target, nil)
	if err != nil {
		return false
	}
	client := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close() //nolint:errcheck
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// pairingView is what the admin pairing endpoint returns. The curl command
// is pre-formatted so the SPA renders a copy-paste box rather than asking
// the operator to assemble the URL themselves.
type pairingView struct {
	Token     string `json:"token"`
	ExpiresIn int    `json:"expires_in_seconds"`
	Curl      string `json:"curl"`
}

// unpairView is what the admin Unpair endpoint returns. CleanupCmd is the
// shell snippet the operator runs on the GX10 itself to stop the systemd
// units — Nimbus can't reach into the GX10 to do this for them (the
// pairing flow is GX10-pulls-from-Nimbus, never the reverse).
type unpairView struct {
	CancelledJobs int    `json:"cancelled_jobs"`
	CleanupCmd    string `json:"cleanup_cmd"`
}

// Unpair handles POST /api/settings/gpu/unpair (admin-only). Flow:
//
//  1. Cancel every queued/running GPU job — no worker will claim them after
//     unpair, so leaving them alive just hides them in the table.
//  2. Wipe the GPU settings (worker token, base URL, model, hostname).
//  3. Notify the live provision config applier so freshly provisioned VMs
//     stop receiving the now-stale OPENAI_BASE_URL.
//
// The handler returns a cleanup-command string the SPA shows the operator
// for the GX10-side teardown. Idempotent: calling Unpair on an unpaired
// instance is a successful no-op (0 jobs cancelled).
func (h *GPU) Unpair(w http.ResponseWriter, r *http.Request) {
	n, err := h.svc.CancelAllNonTerminal(r.Context())
	if err != nil {
		// Logged but not fatal — the unpair should still proceed so the
		// operator can recover from a broken state.
		response.InternalError(w, "cancel jobs failed: "+err.Error())
		return
	}
	if err := h.auth.UnpairGPU(); err != nil {
		response.InternalError(w, "failed to clear GPU settings: "+err.Error())
		return
	}
	if h.gpuConfigApplier != nil {
		// Empty strings → provision flow falls through the gpuCfg.BaseURL
		// guard and skips the GPU bootstrap on subsequent VMs.
		h.gpuConfigApplier("", "", "")
	}
	response.Success(w, unpairView{
		CancelledJobs: n,
		CleanupCmd: "sudo systemctl disable --now nimbus-vllm nimbus-gpu-worker && " +
			"sudo rm -f /etc/systemd/system/nimbus-vllm.service " +
			"/etc/systemd/system/nimbus-gpu-worker.service /etc/nimbus-gpu-worker.env && " +
			"sudo systemctl daemon-reload",
	})
}

// MintPairing handles POST /api/settings/gpu/pairing (admin-only). Mints a
// fresh 5-min pairing token and returns the curl command the operator
// pastes onto the GX10. Replaces any active pairing token; we only support
// pairing one GX10 at a time.
//
// Fails with 412 when neither r.Host nor APP_URL is publicly reachable —
// otherwise we'd hand back a curl with `localhost` baked in, which the
// GX10 obviously can't reach. The operator either browses Nimbus at its
// LAN/public hostname or sets APP_URL before retrying.
func (h *GPU) MintPairing(w http.ResponseWriter, r *http.Request) {
	base := h.resolveBase(r)
	if base == "" {
		response.Error(w, http.StatusPreconditionFailed,
			"Nimbus appears to be accessed via localhost — the GX10 can't reach a localhost URL. "+
				"Either browse Nimbus at its LAN / public hostname before clicking Add GX10, "+
				"or set APP_URL in Nimbus's environment to a publicly-reachable URL and restart.")
		return
	}
	tok, err := h.auth.MintGPUPairingToken()
	if err != nil {
		response.InternalError(w, "failed to mint pairing token")
		return
	}
	// Pipe-form rather than process-substitution: `sudo bash <(curl ...)`
	// fails on some distros (notably recent Ubuntu / DGX OS) because sudo
	// strips the /dev/fd/N descriptor across the privilege boundary,
	// producing a cryptic "/dev/fd/63: No such file or directory". Reading
	// the script from stdin sidesteps that — sudo preserves stdin cleanly,
	// and the script is non-interactive so we don't lose stdin to anything
	// important inside it.
	curl := fmt.Sprintf(`curl -fsSL %q | sudo bash`,
		base+"/api/gpu/install.sh?token="+tok)
	response.Success(w, pairingView{
		Token:     tok,
		ExpiresIn: 5 * 60,
		Curl:      curl,
	})
}

// InstallScript handles GET /api/gpu/install.sh?token=<pairing>. Public —
// the pairing token IS the auth. Returns a bash script that, when run on
// the GX10, posts to /api/gpu/register to exchange the pairing token for a
// worker token, then runs install-inference.sh + install-worker.sh.
//
// The pairing token alone doesn't carry permissions — it can only be
// consumed once, and only inside its 5-min window. Leaking the URL
// post-expiry is harmless.
func (h *GPU) InstallScript(w http.ResponseWriter, r *http.Request) {
	pairing := r.URL.Query().Get("token")
	if !h.auth.VerifyGPUPairingToken(pairing) {
		response.Error(w, http.StatusUnauthorized,
			"invalid or expired pairing token — admin must click 'Add GX10' in Settings → GPU")
		return
	}
	// At this point the GX10 successfully reached us, so r.Host is already
	// proven-reachable. Use it directly — no need to re-fall-back to AppURL.
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	base := scheme + "://" + r.Host
	// All four substitutions are: nimbus URL, pairing token, nimbus URL, nimbus URL.
	// Splitting the format string from the heredoc body keeps the Go-side
	// template readable.
	script := fmt.Sprintf(`#!/usr/bin/env bash
# Nimbus GX10 bootstrap — pairs this host with Nimbus, installs vLLM as
# nimbus-vllm.service, and installs the Nimbus job worker as
# nimbus-gpu-worker.service. Idempotent.
#
# Override before piping if you want different defaults:
#   curl ... | GX10_INFERENCE_MODEL=mistralai/Mistral-7B-Instruct-v0.3 sudo -E bash
#   curl ... | GX10_INFERENCE_PORT=8000 sudo -E bash
set -euo pipefail

NIMBUS_URL=%q
PAIRING_TOKEN=%q

MODEL="${GX10_INFERENCE_MODEL:-microsoft/Phi-3-mini-4k-instruct}"
PORT="${GX10_INFERENCE_PORT:-8000}"

if [ "$(id -u)" -ne 0 ]; then
  echo "must run as root (try the same command with sudo)" >&2
  exit 1
fi

# Detect this host's primary outbound IP — Nimbus uses this as the inference
# base URL it injects into every VM, so we want the address that's reachable
# from the cluster LAN. "hostname -I" gives all addresses; the first one is
# typically the primary, but operators on multi-NIC boxes may want to set
# GX10_HOST_IP explicitly.
HOST_IP="${GX10_HOST_IP:-$(hostname -I | awk '{print $1}')}"
HOSTNAME_REPORTED="$(hostname)"

echo "==> registering with Nimbus (ip=$HOST_IP model=$MODEL)"
RESP="$(curl -fsSL -X POST "${NIMBUS_URL}/api/gpu/register" \
  -H 'Content-Type: application/json' \
  -d "{\"pairing_token\":\"${PAIRING_TOKEN}\",\"hostname\":\"${HOSTNAME_REPORTED}\",\"ip\":\"${HOST_IP}\",\"model\":\"${MODEL}\",\"port\":${PORT}}")"
WORKER_TOKEN="$(printf '%%s' "$RESP" | python3 -c 'import json,sys;print(json.load(sys.stdin)["data"]["worker_token"])')"
if [ -z "$WORKER_TOKEN" ]; then
  echo "registration failed: $RESP" >&2
  exit 1
fi
echo "==> registered, received worker token"

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

echo "==> downloading install-inference.sh"
curl -fsSL "${NIMBUS_URL}/api/gpu/scripts/install-inference.sh" -o "$TMPDIR/install-inference.sh"
echo "==> downloading install-worker.sh"
curl -fsSL "${NIMBUS_URL}/api/gpu/scripts/install-worker.sh" -o "$TMPDIR/install-worker.sh"
chmod +x "$TMPDIR/install-inference.sh" "$TMPDIR/install-worker.sh"

NIMBUS_URL="$NIMBUS_URL" GX10_INFERENCE_MODEL="$MODEL" GX10_INFERENCE_PORT="$PORT" \
  bash "$TMPDIR/install-inference.sh"
NIMBUS_URL="$NIMBUS_URL" NIMBUS_WORKER_TOKEN="$WORKER_TOKEN" \
  bash "$TMPDIR/install-worker.sh"

echo ""
echo "==> done."
echo "    inference: systemctl status nimbus-vllm"
echo "    worker:    systemctl status nimbus-gpu-worker"
echo "    base url:  http://${HOST_IP}:${PORT}/v1"
`, base, pairing)
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(script))
}

type gpuRegisterRequest struct {
	PairingToken string `json:"pairing_token"`
	Hostname     string `json:"hostname"`
	IP           string `json:"ip"`
	Model        string `json:"model"`
	Port         int    `json:"port"`
}

type gpuRegisterResponse struct {
	WorkerToken string `json:"worker_token"`
	BaseURL     string `json:"base_url"`
}

// Register handles POST /api/gpu/register (public, pairing-token-auth).
// Trades a valid pairing token for a fresh worker token, recording the
// GX10's self-reported IP + model. Single use: a successful register
// clears the pairing token, so the GX10 effectively "claims" the seat.
func (h *GPU) Register(w http.ResponseWriter, r *http.Request) {
	var req gpuRegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON body")
		return
	}
	if !h.auth.VerifyGPUPairingToken(req.PairingToken) {
		response.Error(w, http.StatusUnauthorized,
			"invalid or expired pairing token")
		return
	}
	if req.IP == "" {
		response.BadRequest(w, "ip is required")
		return
	}
	tok, err := h.auth.RegisterGPU(req.Hostname, req.IP, req.Model, req.Port)
	if err != nil {
		response.InternalError(w, "registration failed: "+err.Error())
		return
	}
	// Push the fresh config to anything that holds a live copy
	// (provision.Service for cloud-init env injection). Derive
	// nimbusGPUAPI from the request host — the GX10 just successfully
	// reached us at r.Host, so it's proven-routable. Sidesteps a
	// misconfigured APP_URL (which would default to localhost:5173 on
	// fresh installs and poison the bootstrap config).
	if h.gpuConfigApplier != nil {
		settings, err := h.auth.GetGPUSettings()
		if err == nil {
			scheme := "http"
			if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
				scheme = "https"
			}
			nimbusGPUAPI := scheme + "://" + r.Host + "/api/gpu"
			h.gpuConfigApplier(settings.BaseURL, settings.InferenceModel, nimbusGPUAPI)
		}
	}
	response.Success(w, gpuRegisterResponse{
		WorkerToken: tok,
		BaseURL:     fmt.Sprintf("http://%s:%d", req.IP, defaultIfZero(req.Port, 8000)),
	})
}

// resolveBase picks the base URL the GX10 should phone home to. Priority:
//
//  1. The inbound request's Host header, when it's a real hostname. This
//     is what the operator's browser used to reach Nimbus, so it's the URL
//     most likely reachable from the GX10 too — including any reverse
//     proxy / CF tunnel in front.
//  2. The configured AppURL, when r.Host is localhost-ish (typical dev
//     setup with Vite at :5173 proxying to Nimbus at :8080). AppURL must
//     itself be a real hostname; the localhost default is rejected.
//
// Returns "" when no usable base could be derived — the caller surfaces
// a clear error so the operator knows to set APP_URL or browse Nimbus at
// its LAN/public hostname before pairing.
func (h *GPU) resolveBase(r *http.Request) string {
	if !isLocalhostHost(r.Host) {
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		return scheme + "://" + r.Host
	}
	if h.nimbusBaseURL != "" && !isLocalhostURL(h.nimbusBaseURL) {
		return h.nimbusBaseURL
	}
	return ""
}

func isLocalhostHost(host string) bool {
	h := strings.ToLower(host)
	if h == "" {
		return true // empty Host is unusable, treat like localhost
	}
	// Strip optional port for the prefix check.
	if i := strings.LastIndex(h, ":"); i >= 0 && !strings.Contains(h[:i], ":") {
		h = h[:i]
	}
	return h == "localhost" ||
		strings.HasPrefix(h, "127.") ||
		h == "0.0.0.0" ||
		h == "::1" ||
		h == "[::1]"
}

func isLocalhostURL(url string) bool {
	u := strings.ToLower(url)
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if i := strings.IndexAny(u, "/?#"); i >= 0 {
		u = u[:i]
	}
	return isLocalhostHost(u)
}

func defaultIfZero(n, fallback int) int {
	if n == 0 {
		return fallback
	}
	return n
}

// ScriptHandler serves a static file from the embedded gx10 asset bundle.
// Assets are baked into the binary via //go:embed in cmd/server/main.go
// (populated by `make gx10-bundle` before build) so the deployed Nimbus
// is self-contained and doesn't depend on the source tree being present
// next to the running binary.
//
// We accept a filename whitelist to avoid path-traversal: only the
// known install assets are servable.
type ScriptHandler struct {
	assets fs.FS
}

// NewScriptHandler returns a handler for GET /api/gpu/scripts/{name}.
func NewScriptHandler(assets fs.FS) *ScriptHandler {
	return &ScriptHandler{assets: assets}
}

var allowedScripts = map[string]bool{
	"install-inference.sh": true,
	"install-worker.sh":    true,
	"gx10-worker":          true, // ARM64 binary; Worker downloads it
	"demo-mnist.py":        true, // Phase 4 smoke-test; safe to expose, no secrets
}

func (h *ScriptHandler) Serve(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if !allowedScripts[name] {
		http.NotFound(w, r)
		return
	}
	if h.assets == nil {
		http.NotFound(w, r)
		return
	}
	// The embed FS preserves the directory layout — the bundle was staged
	// at cmd/server/gx10-assets/, so within the embed it's "gx10-assets/<name>".
	f, err := h.assets.Open("gx10-assets/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close() //nolint:errcheck
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.Copy(w, f)
}

// ----------------- worker-facing routes -----------------

// ClaimNext handles POST /api/gpu/worker/claim. 200 with one job, or 204.
func (h *GPU) ClaimNext(w http.ResponseWriter, r *http.Request) {
	workerID := r.Header.Get("X-Worker-ID")
	if workerID == "" {
		workerID = "unknown"
	}
	job, ok, err := h.svc.ClaimNextJob(r.Context(), workerID)
	if err != nil {
		response.FromError(w, err)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	response.Success(w, toGPUJobView(job))
}

// AppendLogs handles POST /api/gpu/worker/jobs/{id}/logs. Body is raw
// stdout+stderr; we don't parse, just append. 1 MB per call cap to keep
// SQLite happy and prevent a runaway client from filling /var/lib.
const maxLogChunk = 1 << 20 // 1 MB

func (h *GPU) AppendLogs(w http.ResponseWriter, r *http.Request) {
	id, err := parseUintParam(r, "id")
	if err != nil {
		response.BadRequest(w, "invalid id")
		return
	}
	chunk, err := io.ReadAll(io.LimitReader(r.Body, maxLogChunk+1))
	if err != nil {
		response.BadRequest(w, "failed to read body")
		return
	}
	if len(chunk) > maxLogChunk {
		response.Error(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("log chunk exceeds %d bytes", maxLogChunk))
		return
	}
	if err := h.svc.AppendLogs(r.Context(), id, chunk); err != nil {
		response.FromError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type workerStatusRequest struct {
	Status       string `json:"status"`
	ExitCode     int    `json:"exit_code"`
	ArtifactPath string `json:"artifact_path,omitempty"`
	ErrorMsg     string `json:"error_msg,omitempty"`
}

// ReportStatus handles POST /api/gpu/worker/jobs/{id}/status.
func (h *GPU) ReportStatus(w http.ResponseWriter, r *http.Request) {
	id, err := parseUintParam(r, "id")
	if err != nil {
		response.BadRequest(w, "invalid id")
		return
	}
	var req workerStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON body")
		return
	}
	if err := h.svc.ReportStatus(r.Context(), id, gpu.ReportStatusRequest{
		Status:       req.Status,
		ExitCode:     req.ExitCode,
		ArtifactPath: req.ArtifactPath,
		ErrorMsg:     req.ErrorMsg,
	}); err != nil {
		response.FromError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ----------------- helpers -----------------

func parseUintParam(r *http.Request, name string) (uint, error) {
	raw := chi.URLParam(r, name)
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, err
	}
	return uint(n), nil
}
