package handlers

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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
	// a Nimbus restart. Nil is allowed (handler still works; provision
	// flow refreshes on next startup).
	gpuConfigApplier func(baseURL, model string)
}

// NewGPU wires the handler.
func NewGPU(svc *gpu.Service, auth *service.AuthService, nimbusBaseURL string) *GPU {
	return &GPU{svc: svc, auth: auth, nimbusBaseURL: strings.TrimRight(nimbusBaseURL, "/")}
}

// WithGPUConfigApplier installs the post-register callback. Builder-style so
// router wiring stays a single fluent expression.
func (h *GPU) WithGPUConfigApplier(fn func(baseURL, model string)) *GPU {
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
		OwnerID: user.ID,
		VMID:    req.VMID,
		Image:   req.Image,
		Command: req.Command,
		Env:     req.Env,
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

// MintPairing handles POST /api/settings/gpu/pairing (admin-only). Mints a
// fresh 5-min pairing token and returns the curl command the operator
// pastes onto the GX10. Replaces any active pairing token; we only support
// pairing one GX10 at a time.
func (h *GPU) MintPairing(w http.ResponseWriter, r *http.Request) {
	tok, err := h.auth.MintGPUPairingToken()
	if err != nil {
		response.InternalError(w, "failed to mint pairing token")
		return
	}
	base := h.resolveBase(r)
	curl := fmt.Sprintf(`sudo bash <(curl -fsSL %q)`,
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
	base := h.resolveBase(r)
	// All four substitutions are: nimbus URL, pairing token, nimbus URL, nimbus URL.
	// Splitting the format string from the heredoc body keeps the Go-side
	// template readable.
	script := fmt.Sprintf(`#!/usr/bin/env bash
# Nimbus GX10 bootstrap — pairs this host with Nimbus, installs vLLM as
# nimbus-vllm.service, and installs the Nimbus job worker as
# nimbus-gpu-worker.service. Idempotent.
#
# Override before piping if you want different defaults:
#   GX10_INFERENCE_MODEL=mistralai/Mistral-7B-Instruct-v0.3 sudo bash <(...)
#   GX10_INFERENCE_PORT=8000
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
	// (provision.Service for cloud-init env injection).
	if h.gpuConfigApplier != nil {
		settings, err := h.auth.GetGPUSettings()
		if err == nil {
			h.gpuConfigApplier(settings.BaseURL, settings.InferenceModel)
		}
	}
	response.Success(w, gpuRegisterResponse{
		WorkerToken: tok,
		BaseURL:     fmt.Sprintf("http://%s:%d", req.IP, defaultIfZero(req.Port, 8000)),
	})
}

// resolveBase picks the base URL Nimbus knows about, falling back to the
// inbound request's host when AppURL hasn't been set. Lets the install
// script work on a freshly-set-up Nimbus where the operator hasn't
// hardcoded an external URL yet.
func (h *GPU) resolveBase(r *http.Request) string {
	if h.nimbusBaseURL != "" {
		return h.nimbusBaseURL
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func defaultIfZero(n, fallback int) int {
	if n == 0 {
		return fallback
	}
	return n
}

// ScriptHandler serves a static file from the gx10 scripts dir. The file
// is embedded at compile time via the cmd/server frontend embed; we mount
// a separate FS from disk so operators can iterate without recompiling
// during development.
//
// We accept a filename whitelist to avoid path-traversal: only the two
// known install scripts are servable.
type ScriptHandler struct {
	scriptDir string
}

// NewScriptHandler returns a handler for GET /api/gpu/scripts/{name}.
func NewScriptHandler(scriptDir string) *ScriptHandler {
	return &ScriptHandler{scriptDir: scriptDir}
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
	path := h.scriptDir + "/" + name
	f, err := openScript(path)
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

// openScript opens a script file for serving. Pulled out so tests can inject
// a fake FS later if needed; for now it's just os.Open.
func openScript(path string) (io.ReadCloser, error) {
	return os.Open(path) //nolint:gosec // path comes from a closed allowlist (allowedScripts)
}
