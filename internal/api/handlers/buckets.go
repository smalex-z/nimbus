package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"nimbus/internal/api/response"
	"nimbus/internal/ctxutil"
	"nimbus/internal/s3storage"
)

// Buckets is the user-facing surface for the singleton MinIO host. Each
// verified user manages their own buckets via four endpoints; per-user
// isolation is enforced server-side by the prefixed bucket-naming
// invariant in s3storage.UserBucketService.
type Buckets struct {
	svc *s3storage.UserBucketService
}

func NewBuckets(svc *s3storage.UserBucketService) *Buckets {
	return &Buckets{svc: svc}
}

// userBucketView is the per-bucket payload returned in the list response.
// Mirrors s3storage.BucketStat but uses the json tag shape the SPA expects.
type userBucketView struct {
	Name           string `json:"name"`
	CreatedAt      string `json:"created_at"`
	ObjectCount    int64  `json:"object_count"`
	TotalSizeBytes int64  `json:"total_size_bytes"`
}

// credentialsView is the wire shape of GET /api/buckets/credentials.
type credentialsView struct {
	Endpoint  string `json:"endpoint"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	Prefix    string `json:"prefix"`
}

// createBucketBody is the input shape for POST /api/buckets. Only the user-
// typed half of the bucket name comes over the wire; the prefix half is
// composed server-side from the calling user's service-account row.
type createBucketBody struct {
	Name string `json:"name" example:"uploads"`
}

// createBucketResp is the success shape for POST /api/buckets.
type createBucketResp struct {
	Name string `json:"name" example:"kevin-u3-uploads"`
}

// deleteBucketResp is the success shape for DELETE /api/buckets/{name}.
type deleteBucketResp struct {
	Message string `json:"message" example:"bucket deleted"`
}

// List handles GET /api/buckets. Returns only the calling user's buckets;
// cross-user listings are not exposed at any endpoint.
//
// @Summary     List the calling user's MinIO buckets
// @Description Bucket name, creation time, object count, and total size for
// @Description each bucket owned by the caller. Returns 503 with a stable
// @Description substring ("no s3 storage deployed" / "not ready") when the
// @Description shared storage host hasn't been deployed by an admin yet —
// @Description the SPA distinguishes these from real errors and renders an
// @Description empty-state card.
// @Tags        buckets
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=[]userBucketView}
// @Failure     401 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Failure     503 {object} EnvelopeError
// @Router      /buckets [get]
func (h *Buckets) List(w http.ResponseWriter, r *http.Request) {
	uid, ok := requesterID(w, r)
	if !ok {
		return
	}
	stats, err := h.svc.List(r.Context(), uid)
	if err != nil {
		writeBucketsServiceError(w, err)
		return
	}
	out := make([]userBucketView, 0, len(stats))
	for _, s := range stats {
		out = append(out, userBucketView{
			Name:           s.Name,
			CreatedAt:      s.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			ObjectCount:    s.ObjectCount,
			TotalSizeBytes: s.TotalSize,
		})
	}
	response.Success(w, out)
}

// Create handles POST /api/buckets.
//
// @Summary     Create a bucket owned by the calling user
// @Description The user supplies only the suffix half of the bucket name
// @Description (3-30 chars, lowercase alnum + hyphen, no leading/trailing
// @Description hyphen). Server composes the full name as
// @Description `<owner-prefix>-<suffix>` so cross-user collisions are
// @Description impossible. Service account is auto-minted on first call
// @Description if the user doesn't already have one.
// @Tags        buckets
// @Security    cookieAuth
// @Accept      json
// @Produce     json
// @Param       body body createBucketBody true "Bucket suffix"
// @Success     201 {object} EnvelopeOK{data=createBucketResp}
// @Failure     400 {object} EnvelopeError
// @Failure     401 {object} EnvelopeError
// @Failure     409 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Failure     503 {object} EnvelopeError
// @Router      /buckets [post]
func (h *Buckets) Create(w http.ResponseWriter, r *http.Request) {
	user := ctxutil.User(r.Context())
	if user == nil {
		response.Error(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	var body createBucketBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		response.BadRequest(w, "invalid JSON body")
		return
	}
	row, err := h.svc.Create(r.Context(), user.ID, user.Name, body.Name)
	if err != nil {
		if errors.Is(err, s3storage.ErrBucketNameInvalid) {
			response.BadRequest(w, err.Error())
			return
		}
		if errors.Is(err, s3storage.ErrBucketAlreadyExists) {
			response.Conflict(w, "a bucket with this name already exists")
			return
		}
		writeBucketsServiceError(w, err)
		return
	}
	response.Created(w, createBucketResp{Name: row.Name})
}

// Delete handles DELETE /api/buckets/{name}.
//
// @Summary     Delete a bucket owned by the calling user
// @Description 404 if the bucket isn't owned by the caller (existence is not
// @Description disclosed across ownership boundaries). 409 if the bucket is
// @Description not empty — the user must empty it first; Nimbus does not
// @Description force-delete object data.
// @Tags        buckets
// @Security    cookieAuth
// @Produce     json
// @Param       name path string true "Bucket name (full composed form)"
// @Success     200 {object} EnvelopeOK{data=deleteBucketResp}
// @Failure     401 {object} EnvelopeError
// @Failure     404 {object} EnvelopeError
// @Failure     409 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Failure     503 {object} EnvelopeError
// @Router      /buckets/{name} [delete]
func (h *Buckets) Delete(w http.ResponseWriter, r *http.Request) {
	uid, ok := requesterID(w, r)
	if !ok {
		return
	}
	name := chi.URLParam(r, "name")
	if name == "" {
		response.BadRequest(w, "missing bucket name")
		return
	}
	if err := h.svc.Delete(r.Context(), uid, name); err != nil {
		if errors.Is(err, s3storage.ErrBucketNotOwned) {
			response.NotFound(w, "bucket not found")
			return
		}
		if errors.Is(err, s3storage.ErrBucketNotEmpty) {
			response.Conflict(w, "bucket is not empty — empty it first")
			return
		}
		writeBucketsServiceError(w, err)
		return
	}
	response.Success(w, deleteBucketResp{Message: "bucket deleted"})
}

// Credentials handles GET /api/buckets/credentials. Auto-mints a service
// account on first call so the user can copy creds into their app before
// (or instead of) creating a bucket via the UI.
//
// @Summary     Read the calling user's MinIO service-account credentials
// @Description Endpoint + access key + secret key + bucket name prefix the
// @Description user's policy allows. The secret is returned every time;
// @Description treat it as the primary share surface, not a one-time view.
// @Description Auto-mints the service account on first call if needed.
// @Tags        buckets
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=credentialsView}
// @Failure     401 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Failure     503 {object} EnvelopeError
// @Router      /buckets/credentials [get]
func (h *Buckets) Credentials(w http.ResponseWriter, r *http.Request) {
	user := ctxutil.User(r.Context())
	if user == nil {
		response.Error(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	creds, err := h.svc.Credentials(r.Context(), user.ID, user.Name)
	if err != nil {
		writeBucketsServiceError(w, err)
		return
	}
	response.Success(w, credentialsView{
		Endpoint:  creds.Endpoint,
		AccessKey: creds.AccessKey,
		SecretKey: creds.SecretKey,
		Prefix:    creds.Prefix,
	})
}

// writeBucketsServiceError adapts s3storage error sentinels to HTTP
// statuses. Storage-not-deployed and storage-not-ready both surface as
// 503 with stable substrings the SPA matches on to render the
// empty-state card.
func writeBucketsServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, s3storage.ErrNotDeployed):
		response.ServiceUnavailable(w, "no s3 storage deployed")
	case errors.Is(err, s3storage.ErrNotReady):
		response.ServiceUnavailable(w, "s3 storage is not ready yet")
	default:
		response.InternalError(w, err.Error())
	}
}
