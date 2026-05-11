package proxmox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// UploadFile uploads a file to a node's storage with the named
// content type. Proxmox 8.x and 9.x accept `iso`, `vztmpl`, and
// `import` — `snippets` is NOT accepted by this endpoint, contrary
// to some older docs (verified empirically and against PVE perl
// source).
//
// The body has exactly two multipart parts:
//   - `content` field = e.g. "iso"
//   - `filename` file part — Proxmox extracts the storage filename
//     from Content-Disposition's `filename=` attribute on this part.
//
// We deliberately do NOT also send `filename` as a string field;
// duplicate-key form data confused Proxmox's perl parser
// (last-write-wins) on some PVE versions, occasionally landing the
// file under the wrong name.
//
// IMPORTANT: PVE's upload endpoint is asynchronous — it forks a
// worker and returns a UPID immediately, before the file is on
// disk. We block on the UPID via WaitForTask so the caller can
// safely reference the volid right away (e.g. AttachCDROM as the
// next step). Without this wait, Proxmox responds with "volume does
// not exist" on the next API call that touches the volid.
func (c *Client) UploadFile(ctx context.Context, node, storage, contentType, filename string, content []byte) error {
	if filename == "" {
		return fmt.Errorf("upload: empty filename")
	}
	if contentType == "" {
		return fmt.Errorf("upload: empty content type")
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("content", contentType); err != nil {
		return fmt.Errorf("upload: write content field: %w", err)
	}
	fw, err := mw.CreateFormFile("filename", filename)
	if err != nil {
		return fmt.Errorf("upload: create file part: %w", err)
	}
	if _, err := fw.Write(content); err != nil {
		return fmt.Errorf("upload: write file body: %w", err)
	}
	if err := mw.Close(); err != nil {
		return fmt.Errorf("upload: close multipart: %w", err)
	}

	endpoint := c.host + "/api2/json/nodes/" + url.PathEscape(node) +
		"/storage/" + url.PathEscape(storage) + "/upload"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return fmt.Errorf("upload: build request: %w", err)
	}
	req.Header.Set("Authorization", c.authHdr)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("upload %s/%s on %s: %w", storage, filename, node, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{
			Status: resp.StatusCode,
			Method: http.MethodPost,
			Path:   "/nodes/" + node + "/storage/" + storage + "/upload",
			Body:   string(respBody),
		}
	}

	// Parse the UPID and wait for the upload task to complete. Older
	// PVE / non-task responses return a non-string `data` (or no UPID
	// shape) — in that case we assume synchronous completion and
	// return nil so we don't break anything that worked before.
	//
	// Critical: the worker UPID may report a DIFFERENT node than the
	// node we POSTed to. On a cluster where storage is shared or
	// where PVE routes the imgcopy worker to whichever node already
	// has the source data, the response can carry e.g.
	// `UPID:sixseven:...` even though the upload was POSTed to
	// `neanderthal`. /nodes/{node}/tasks/{upid}/status is NOT
	// cluster-aware — it 400s "no such task" when {node} doesn't
	// match the UPID's node prefix. So we must poll on the node
	// embedded in the UPID, not the upload-target node.
	upid := parseUploadUPID(respBody)
	if upid == "" {
		return nil
	}
	taskNode := nodeFromUPID(upid)
	if taskNode == "" {
		taskNode = node // defensive fallback for malformed UPIDs
	}
	if err := c.WaitForTask(ctx, taskNode, upid, 500*time.Millisecond); err != nil {
		return fmt.Errorf("upload %s/%s on %s: wait task: %w", storage, filename, node, err)
	}
	return nil
}

// parseUploadUPID extracts the UPID string from PVE's upload
// response envelope, e.g. {"data":"UPID:node:..."}. Returns empty
// string when the response shape is unexpected (older PVE versions
// or storage backends that respond synchronously).
func parseUploadUPID(body []byte) string {
	var env struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	if !strings.HasPrefix(env.Data, "UPID:") {
		return ""
	}
	return env.Data
}

// nodeFromUPID extracts the node name from a Proxmox UPID string.
// UPID format is `UPID:nodename:pid:starttime:tstime:type:id:user:`
// — the nodename is the second colon-separated field. Returns empty
// string when the input doesn't look like a UPID; callers should
// fall back to a known node in that case.
func nodeFromUPID(upid string) string {
	if !strings.HasPrefix(upid, "UPID:") {
		return ""
	}
	rest := upid[len("UPID:"):]
	if i := strings.IndexByte(rest, ':'); i > 0 {
		return rest[:i]
	}
	return ""
}

// DeleteStorageVolume removes a single volume (file) from a node's
// storage by its volid. The volid format is the Proxmox canonical
// form `<storage>:<contentType>/<filename>` (e.g.
// `local:iso/nimbus-vm-137.iso`).
//
// Returns nil if the volume is already gone (404 → ErrNotFound,
// swallowed here since cleanup is idempotent). Used by the
// provision flow to clean up per-VM cloud-init ISOs when a VM is
// destroyed; harmless to call against a missing volid.
func (c *Client) DeleteStorageVolume(ctx context.Context, node, volid string) error {
	path := fmt.Sprintf("/nodes/%s/storage/%s/content/%s",
		url.PathEscape(node), url.PathEscape(storageOfVolid(volid)), url.PathEscape(volid))
	if err := c.do(ctx, http.MethodDelete, path, nil, nil); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	return nil
}

// AttachCDROM sets a VM config slot (e.g. "ide2", "sata3") to the
// given storage volume mounted as a CD-ROM. Used by Nimbus to
// attach the per-VM cloud-init ISO at the same slot Proxmox would
// use for its own auto-generated cloud-init drive (ide2 by
// convention).
//
// IMPORTANT: this does NOT cleanly replace an existing
// cloud-init-content drive at the same slot. PVE's config endpoint
// silently keeps the cloudinit volume and ignores the new value
// when the prior slot held a `<storage>:cloudinit` reference. Call
// DetachDrive first if there's any chance the slot is occupied by
// a cloudinit drive (e.g. cloned from a template that had one).
//
// volid is the canonical Proxmox form `<storage>:iso/<filename>`.
func (c *Client) AttachCDROM(ctx context.Context, node string, vmid int, slot, volid string) error {
	if slot == "" {
		return fmt.Errorf("attach cdrom: empty slot")
	}
	params := url.Values{}
	params.Set(slot, fmt.Sprintf("%s,media=cdrom", volid))
	path := fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(node), vmid)
	return c.do(ctx, http.MethodPost, path, params, nil)
}

// AttachCloudInitDrive sets the given slot (typically "ide2") to a
// Proxmox-managed cloud-init drive backed by the named storage. The
// volid `<storage>:cloudinit` is a magic Proxmox value: instead of
// referencing an existing volume, it tells PVE to generate a small
// per-VM ISO whose contents are derived from the VM's config fields
// (`ciuser`, `cipassword`, `sshkeys`, `ipconfig0`, …). The drive is
// regenerated on every relevant config change.
//
// Use this rather than AttachCDROM when you want the managed-drive
// semantics — AttachCDROM appends `media=cdrom` and treats the slot
// as a static image, which prevents Proxmox from rewriting it when
// SetCloudInit runs.
//
// The choice of storage matters operationally but not semantically:
// Nimbus passes the boot disk's storage so the cloudinit drive lives
// on the same volume as the VM, ensuring it travels (or gets
// regenerated) wherever the VM lives. Proxmox handles migration of
// the managed drive natively — no per-node-local CDROM blockage.
func (c *Client) AttachCloudInitDrive(ctx context.Context, node string, vmid int, slot, storage string) error {
	if slot == "" {
		return fmt.Errorf("attach cloudinit: empty slot")
	}
	if storage == "" {
		return fmt.Errorf("attach cloudinit: empty storage")
	}
	params := url.Values{}
	params.Set(slot, fmt.Sprintf("%s:cloudinit", storage))
	path := fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(node), vmid)
	return c.do(ctx, http.MethodPost, path, params, nil)
}

// DetachDrive removes a VM config slot (e.g. ide2, sata1) and the
// underlying volume if Proxmox manages it. Implemented as POST
// /config with `delete=<slot>` — same channel Proxmox's web UI uses
// when an admin clicks "Remove" on a drive.
//
// Used by the provision flow before AttachCDROM to clean out a
// cloudinit drive that a clone may have inherited from its template.
// Idempotent: calling on an empty slot returns success.
func (c *Client) DetachDrive(ctx context.Context, node string, vmid int, slot string) error {
	if slot == "" {
		return fmt.Errorf("detach drive: empty slot")
	}
	params := url.Values{}
	params.Set("delete", slot)
	path := fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(node), vmid)
	return c.do(ctx, http.MethodPost, path, params, nil)
}

// storageOfVolid extracts the storage portion of a volid like
// "local:iso/foo.iso" → "local". Returns the input unchanged if no
// colon is present (defensive — never observed in practice since
// Proxmox always emits volids in canonical form).
func storageOfVolid(volid string) string {
	if i := indexOfByte(volid, ':'); i >= 0 {
		return volid[:i]
	}
	return volid
}

func indexOfByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
