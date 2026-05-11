package proxmox

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gorilla/websocket"
)

// VNCProxyTicket is what POST /nodes/{node}/qemu/{vmid}/vncproxy returns
// when called with websocket=1. Ticket is the per-session secret bound
// to a single VNC framebuffer socket on the PVE node and is used for
// two things in the SPA: (1) as the `vncticket` query param on the
// vncwebsocket WS upgrade, (2) as the VNC password during the RFB
// handshake's VNC Auth (type 2) phase. Both go through the same string.
type VNCProxyTicket struct {
	Ticket string `json:"ticket"`
	Port   int    `json:"-"` // PVE returns as a string; we coerce.
	UPID   string `json:"upid"`
	User   string `json:"user"`
}

// rawVNCProxyTicket is the wire shape — PVE returns port as a string.
type rawVNCProxyTicket struct {
	Ticket string `json:"ticket"`
	Port   string `json:"port"`
	UPID   string `json:"upid"`
	User   string `json:"user"`
}

// VNCProxy opens a graphical-console (noVNC) session on the PVE node.
// Returns the per-session ticket + port that feed into the subsequent
// vncwebsocket upgrade.
//
// We use vncproxy (not termproxy) deliberately: PVE 9.x rejects API
// token auth at termproxy's in-band ticket handshake — the call hangs,
// the WS closes 1006 ~3s later. vncproxy goes through the standard
// RFB handshake which authenticates end-to-end between the browser's
// noVNC client and PVE's VNC server, bypassing the in-band check
// entirely.
//
// websocket=1 tells PVE the caller intends to bridge via vncwebsocket
// (vs. spawning a real VNC client) so the listener uses the WS-friendly
// codec.
func (c *Client) VNCProxy(ctx context.Context, node string, vmid int) (*VNCProxyTicket, error) {
	var raw rawVNCProxyTicket
	path := fmt.Sprintf("/nodes/%s/qemu/%d/vncproxy", url.PathEscape(node), vmid)
	params := url.Values{}
	params.Set("websocket", "1")
	if err := c.do(ctx, http.MethodPost, path, params, &raw); err != nil {
		return nil, fmt.Errorf("vncproxy %s/%d: %w", node, vmid, err)
	}
	port, err := strconv.Atoi(raw.Port)
	if err != nil {
		return nil, fmt.Errorf("vncproxy %s/%d: parse port %q: %w", node, vmid, raw.Port, err)
	}
	log.Printf("proxmox console: vncproxy %s/%d → user=%q port=%d ticket_len=%d upid=%q",
		node, vmid, raw.User, port, len(raw.Ticket), raw.UPID)
	return &VNCProxyTicket{
		Ticket: raw.Ticket,
		Port:   port,
		UPID:   raw.UPID,
		User:   raw.User,
	}, nil
}

// DialConsoleWS opens the WebSocket to the VM's RFB stream on the PVE
// node. Caller closes the returned conn.
//
// Auth chain: API token via Authorization header authenticates the WS
// upgrade itself. The vncticket query param binds the WS to the
// specific vncproxy session PVE issued. RFB auth (the "password
// challenge" inside the byte stream) is end-to-end between the browser
// and PVE's VNC server — Nimbus does not participate in it; we are a
// transparent byte pump after the upgrade.
//
// Subprotocol must be "binary" — vncwebsocket negotiates binary frames
// for RFB throughout. Without this, PVE accepts the upgrade and
// silently closes ~3s later.
func (c *Client) DialConsoleWS(ctx context.Context, node string, vmid, port int, ticket string) (*websocket.Conn, error) {
	u, err := url.Parse(c.host + "/api2/json/nodes/" + url.PathEscape(node) +
		"/qemu/" + strconv.Itoa(vmid) + "/vncwebsocket")
	if err != nil {
		return nil, fmt.Errorf("build console ws url: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	q := u.Query()
	q.Set("port", strconv.Itoa(port))
	q.Set("vncticket", ticket)
	u.RawQuery = q.Encode()

	dialer := *websocket.DefaultDialer
	dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	dialer.Subprotocols = []string{"binary"}

	hdr := http.Header{}
	hdr.Set("Authorization", c.authHdr)

	conn, resp, err := dialer.DialContext(ctx, u.String(), hdr)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		return nil, fmt.Errorf("dial console ws (status=%d): %w", status, err)
	}
	return conn, nil
}
