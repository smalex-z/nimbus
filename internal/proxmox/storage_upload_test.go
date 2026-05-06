package proxmox_test

import (
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// TestClient_UploadFile_Wire asserts the multipart shape Proxmox
// expects: a `content` field with the type, and a file part named
// `filename` whose Content-Disposition carries the actual bytes.
// We do NOT also send `filename` as a string field — duplicate-key
// form data confused Proxmox's perl parser (last-write-wins) on
// some PVE versions and silently landed files under wrong names.
// Pinning the shape here so a future refactor doesn't reintroduce.
func TestClient_UploadFile_Wire(t *testing.T) {
	t.Parallel()
	var capturedMethod, capturedPath, capturedCT string
	var capturedFields = map[string]string{}
	var capturedFile, capturedFileName string

	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		capturedCT = r.Header.Get("Content-Type")

		_, params, err := mime.ParseMediaType(capturedCT)
		if err != nil {
			t.Errorf("parse media type %q: %v", capturedCT, err)
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("multipart read: %v", err)
				return
			}
			b, _ := io.ReadAll(p)
			if p.FileName() != "" {
				capturedFile = string(b)
				capturedFileName = p.FileName()
			} else {
				capturedFields[p.FormName()] = string(b)
			}
		}
		writeEnvelope(w, "UPID:n:00:00:00:00:storage:upload:done")
	})

	body := []byte("CD001fake-iso-bytes")
	if err := c.UploadFile(context.Background(), "alpha", "local", "iso", "nimbus-vm-100.iso", body); err != nil {
		t.Fatalf("UploadFile: %v", err)
	}

	if capturedMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", capturedMethod)
	}
	if capturedPath != "/api2/json/nodes/alpha/storage/local/upload" {
		t.Errorf("path = %q", capturedPath)
	}
	if !strings.HasPrefix(capturedCT, "multipart/form-data") {
		t.Errorf("Content-Type = %q, want multipart/form-data", capturedCT)
	}
	if capturedFields["content"] != "iso" {
		t.Errorf("content field = %q, want iso", capturedFields["content"])
	}
	if v, ok := capturedFields["filename"]; ok {
		t.Errorf("unexpected string `filename` form field = %q (only the file part should carry the name)", v)
	}
	if capturedFileName != "nimbus-vm-100.iso" {
		t.Errorf("file part filename = %q", capturedFileName)
	}
	if capturedFile != string(body) {
		t.Errorf("file body = %q, want %q", capturedFile, string(body))
	}
}

// TestClient_UploadFile_RejectsEmpty catches the defensive guards
// before the HTTP call — Proxmox would reject these with a generic
// 500.
func TestClient_UploadFile_RejectsEmpty(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	_, c := newMockPVE(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeEnvelope(w, nil)
	})
	if err := c.UploadFile(context.Background(), "n", "local", "iso", "", []byte("x")); err == nil {
		t.Fatal("expected error for empty filename, got nil")
	}
	if err := c.UploadFile(context.Background(), "n", "local", "", "x.iso", []byte("x")); err == nil {
		t.Fatal("expected error for empty content type, got nil")
	}
	if calls.Load() != 0 {
		t.Errorf("server called %d times — guards should reject before HTTP", calls.Load())
	}
}

// TestClient_AttachCDROM_Wire pins the wire shape: a single form
// field at the named slot with `<volid>,media=cdrom`. Proxmox's
// config endpoint accepts this on the ide/sata/scsi config map.
func TestClient_AttachCDROM_Wire(t *testing.T) {
	t.Parallel()
	var capturedPath, capturedBody string
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		writeEnvelope(w, nil)
	})
	if err := c.AttachCDROM(context.Background(), "alpha", 200, "ide2", "local:iso/nimbus-vm-200.iso"); err != nil {
		t.Fatalf("AttachCDROM: %v", err)
	}
	if capturedPath != "/api2/json/nodes/alpha/qemu/200/config" {
		t.Errorf("path = %q", capturedPath)
	}
	want := "ide2=local%3Aiso%2Fnimbus-vm-200.iso%2Cmedia%3Dcdrom"
	if capturedBody != want {
		t.Errorf("body = %q, want %q", capturedBody, want)
	}
}
