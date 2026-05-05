package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"

	"nimbus/internal/api/response"
	"nimbus/internal/db"
	"nimbus/internal/provision"
	"nimbus/internal/s3storage"
)

// S3 wires the s3storage service into the HTTP layer.
type S3 struct {
	svc  *s3storage.Service
	prov *provision.Service
}

func NewS3(svc *s3storage.Service, prov *provision.Service) *S3 {
	return &S3{svc: svc, prov: prov}
}

// bucketNameRE follows AWS S3's stricter bucket-name rules: lowercase
// alphanumerics + hyphens, 3-63 characters, can't start/end with hyphen.
// MinIO accepts a slightly broader set; tightening here keeps the user
// out of "valid in MinIO, invalid in some downstream tool" footguns.
var bucketNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

// storageView is the JSON shape we return from GET /api/s3/storage. We
// expose the root creds so the admin UI can render the "reveal" affordance
// — db.S3Storage's struct tags scrub them from default JSON marshaling.
type storageView struct {
	VMID         int    `json:"vmid"`
	Node         string `json:"node"`
	Status       string `json:"status"`
	DiskGB       int    `json:"disk_gb"`
	Endpoint     string `json:"endpoint,omitempty"`
	RootUser     string `json:"root_user,omitempty"`
	RootPassword string `json:"root_password,omitempty"`
	ErrorMsg     string `json:"error_msg,omitempty"`
}

func toStorageView(row *db.S3Storage) storageView {
	return storageView{
		VMID:         row.VMID,
		Node:         row.Node,
		Status:       row.Status,
		DiskGB:       row.DiskGB,
		Endpoint:     row.Endpoint,
		RootUser:     row.RootUser,
		RootPassword: row.RootPassword,
		ErrorMsg:     row.ErrorMsg,
	}
}

// GetStorage handles GET /api/s3/storage. Returns 404 with a structured
// payload (so the SPA can distinguish "not deployed" from a real error)
// when no row exists.
//
// @Summary     Read the singleton MinIO storage row (admin)
// @Description 404 with a structured payload when no S3 storage is deployed,
// @Description so the SPA can render the deploy form vs the live view.
// @Tags        s3
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=storageView}
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     404 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /s3/storage [get]
func (h *S3) GetStorage(w http.ResponseWriter, _ *http.Request) {
	row, err := h.svc.Get()
	if errors.Is(err, s3storage.ErrNotDeployed) {
		response.NotFound(w, "no s3 storage deployed")
		return
	}
	if err != nil {
		response.InternalError(w, "failed to load s3 storage: "+err.Error())
		return
	}
	response.Success(w, toStorageView(row))
}

// deployStorageRequest is the body of POST /api/s3/storage.
type deployStorageRequest struct {
	DiskGB int `json:"disk_gb"`
}

// CreateStorage handles POST /api/s3/storage as an NDJSON streaming
// response — same shape as POST /api/vms. Each line is one event:
// progress events from the underlying provision flow, then a single
// terminal `result` or `error` line.
//
// @Summary     Deploy the singleton MinIO storage VM (admin)
// @Description Streams NDJSON progress events, terminating in result/error.
// @Description Long-running (up to 15min on cold pull) — the route timeout
// @Description is set generously to accommodate the SSH bootstrap.
// @Tags        s3
// @Security    cookieAuth
// @Accept      json
// @Produce     application/x-ndjson
// @Param       body body deployStorageRequest true "Disk size (10–120 GB)"
// @Success     200  "NDJSON stream of progress events terminating in result or error"
// @Failure     400  {object} EnvelopeError
// @Failure     401  {object} EnvelopeError
// @Failure     403  {object} EnvelopeError
// @Router      /s3/storage [post]
func (h *S3) CreateStorage(w http.ResponseWriter, r *http.Request) {
	var req deployStorageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON body")
		return
	}
	if req.DiskGB < 10 || req.DiskGB > 120 {
		response.BadRequest(w, "disk_gb must be between 10 and 120 (online grow past 120 is a future feature)")
		return
	}

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(w)
	writeLine := func(v any) {
		_ = enc.Encode(v)
		if flusher != nil {
			flusher.Flush()
		}
	}
	reporter := func(evt provision.ProgressEvent) {
		writeLine(map[string]any{
			"type":  "progress",
			"step":  evt.Step,
			"label": evt.Label,
		})
	}

	row, _, err := h.svc.Deploy(r.Context(), h.prov, s3storage.DeployParams{DiskGB: req.DiskGB}, reporter)
	if err != nil {
		writeLine(map[string]any{
			"type":    "error",
			"message": err.Error(),
		})
		return
	}
	writeLine(map[string]any{
		"type": "result",
		"data": toStorageView(row),
	})
}

// deleteStorageResponse is the body of DELETE /api/s3/storage.
type deleteStorageResponse struct {
	Message string `json:"message" example:"s3 storage deleted"`
}

// DeleteStorage handles DELETE /api/s3/storage. Tears down the underlying
// VM, releases its IP, and removes the singleton row. Returns 404 if no
// storage is deployed.
//
// @Summary     Tear down the MinIO storage VM (admin)
// @Tags        s3
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=deleteStorageResponse}
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     404 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /s3/storage [delete]
func (h *S3) DeleteStorage(w http.ResponseWriter, r *http.Request) {
	row, err := h.svc.Get()
	if errors.Is(err, s3storage.ErrNotDeployed) {
		response.NotFound(w, "no s3 storage deployed")
		return
	}
	if err != nil {
		response.InternalError(w, "failed to load s3 storage: "+err.Error())
		return
	}

	if err := h.svc.MarkDeleting(); err != nil {
		response.InternalError(w, "failed to mark deleting: "+err.Error())
		return
	}

	// VMRowID==nil means deploy failed before Provision returned; the VM
	// (if any) was already cleaned up by Provision's own unwind, so we
	// just need to clear the s3_storage row. When the row id is set, the
	// shared admin delete handles stop → destroy → IP release → vms-row
	// purge in one call.
	if row.VMRowID != nil {
		if err := h.prov.AdminDelete(r.Context(), *row.VMRowID); err != nil {
			_ = h.svc.MarkError("destroy failed: " + err.Error())
			response.InternalError(w, "destroy vm: "+err.Error())
			return
		}
	}

	if err := h.svc.Delete(); err != nil {
		response.InternalError(w, "delete row: "+err.Error())
		return
	}
	response.Success(w, deleteStorageResponse{Message: "s3 storage deleted"})
}

// ListBuckets handles GET /api/s3/buckets.
//
// @Summary     List MinIO buckets (admin)
// @Description 503 when storage is absent or not yet ready (deploy in flight,
// @Description or VM still booting). The reconciler converges status; the
// @Description SPA polls until ready.
// @Tags        s3
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=[]s3storage.BucketStat}
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Failure     503 {object} EnvelopeError
// @Router      /s3/buckets [get]
func (h *S3) ListBuckets(w http.ResponseWriter, r *http.Request) {
	bc, err := h.svc.Buckets()
	if err != nil {
		writeBucketsError(w, err)
		return
	}
	stats, err := bc.ListBuckets(r.Context())
	if err != nil {
		response.InternalError(w, "list buckets: "+err.Error())
		return
	}
	if stats == nil {
		stats = []s3storage.BucketStat{}
	}
	response.Success(w, stats)
}

// createBucketRequest is the body of POST /api/s3/buckets.
type createBucketRequest struct {
	Name string `json:"name"`
}

// createBucketResponse is the body of POST /api/s3/buckets.
type createBucketResponse struct {
	Name string `json:"name"`
}

// CreateBucket handles POST /api/s3/buckets.
//
// @Summary     Create a MinIO bucket (admin)
// @Description Bucket name must match the AWS S3 stricter ruleset: 3-63
// @Description chars, lowercase letters/digits/hyphens, no leading/trailing
// @Description hyphen.
// @Tags        s3
// @Security    cookieAuth
// @Accept      json
// @Produce     json
// @Param       body body     createBucketRequest true "Bucket spec"
// @Success     201  {object} EnvelopeOK{data=createBucketResponse}
// @Failure     400  {object} EnvelopeError
// @Failure     401  {object} EnvelopeError
// @Failure     403  {object} EnvelopeError
// @Failure     409  {object} EnvelopeError
// @Failure     500  {object} EnvelopeError
// @Failure     503  {object} EnvelopeError
// @Router      /s3/buckets [post]
func (h *S3) CreateBucket(w http.ResponseWriter, r *http.Request) {
	var req createBucketRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON body")
		return
	}
	if !bucketNameRE.MatchString(req.Name) {
		response.BadRequest(w, "bucket name must be 3-63 chars, lowercase letters/digits/hyphens, not starting or ending with a hyphen")
		return
	}
	bc, err := h.svc.Buckets()
	if err != nil {
		writeBucketsError(w, err)
		return
	}
	if err := bc.CreateBucket(r.Context(), req.Name); err != nil {
		if errors.Is(err, s3storage.ErrBucketAlreadyExists) {
			response.Conflict(w, fmt.Sprintf("bucket %q already exists", req.Name))
			return
		}
		response.InternalError(w, "create bucket: "+err.Error())
		return
	}
	response.Created(w, createBucketResponse(req))
}

// deleteBucketResponse is the body of DELETE /api/s3/buckets/{name}.
type deleteBucketResponse struct {
	Message string `json:"message" example:"bucket deleted"`
}

// DeleteBucket handles DELETE /api/s3/buckets/{name}.
//
// @Summary     Delete a MinIO bucket (admin)
// @Description 409 when the bucket isn't empty — empty it first.
// @Tags        s3
// @Security    cookieAuth
// @Produce     json
// @Param       name path     string true "Bucket name"
// @Success     200  {object} EnvelopeOK{data=deleteBucketResponse}
// @Failure     400  {object} EnvelopeError
// @Failure     401  {object} EnvelopeError
// @Failure     403  {object} EnvelopeError
// @Failure     409  {object} EnvelopeError
// @Failure     500  {object} EnvelopeError
// @Failure     503  {object} EnvelopeError
// @Router      /s3/buckets/{name} [delete]
func (h *S3) DeleteBucket(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if !bucketNameRE.MatchString(name) {
		response.BadRequest(w, "invalid bucket name")
		return
	}
	bc, err := h.svc.Buckets()
	if err != nil {
		writeBucketsError(w, err)
		return
	}
	if err := bc.DeleteBucket(r.Context(), name); err != nil {
		if errors.Is(err, s3storage.ErrBucketNotEmpty) {
			response.Conflict(w, fmt.Sprintf("bucket %q is not empty — empty it first", name))
			return
		}
		response.InternalError(w, "delete bucket: "+err.Error())
		return
	}
	response.Success(w, deleteBucketResponse{Message: "bucket deleted"})
}

// writeBucketsError converts s3storage sentinels to appropriate HTTP
// status codes for any handler that called Service.Buckets().
func writeBucketsError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, s3storage.ErrNotDeployed):
		response.ServiceUnavailable(w, "no s3 storage deployed")
	case errors.Is(err, s3storage.ErrNotReady):
		response.ServiceUnavailable(w, "s3 storage is not ready yet")
	default:
		response.InternalError(w, "minio client unavailable: "+err.Error())
	}
}
