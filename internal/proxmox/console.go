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

// TermProxyTicket is what POST /nodes/{node}/qemu/{vmid}/termproxy returns.
// Ticket is the per-session secret bound to a single VNC/serial socket on
// the PVE node; Port is the localhost port on that node where qemu's
// serial-console multiplexer is listening. Both feed into the subsequent
// vncwebsocket upgrade.
type TermProxyTicket struct {
	Ticket string `json:"ticket"`
	Port   int    `json:"-"` // Proxmox returns "port" as a string; we coerce.
	UPID   string `json:"upid"`
	User   string `json:"user"`
}

// rawTermProxyTicket is the wire shape — Proxmox returns port as a string.
type rawTermProxyTicket struct {
	Ticket string `json:"ticket"`
	Port   string `json:"port"`
	UPID   string `json:"upid"`
	User   string `json:"user"`
}

// TermProxy creates a serial-console proxy session for a VM and returns the
// per-session ticket + listening port. The caller then upgrades a WebSocket
// to /vncwebsocket using these values to stream bytes to/from the VM's
// serial console.
//
// Caller is the user shown in PVE's task log when the session opens.
func (c *Client) TermProxy(ctx context.Context, node string, vmid int) (*TermProxyTicket, error) {
	var raw rawTermProxyTicket
	path := fmt.Sprintf("/nodes/%s/qemu/%d/termproxy", url.PathEscape(node), vmid)
	if err := c.do(ctx, http.MethodPost, path, nil, &raw); err != nil {
		return nil, fmt.Errorf("termproxy %s/%d: %w", node, vmid, err)
	}
	port, err := strconv.Atoi(raw.Port)
	if err != nil {
		return nil, fmt.Errorf("termproxy %s/%d: parse port %q: %w", node, vmid, raw.Port, err)
	}
	log.Printf("proxmox console: termproxy %s/%d → user=%q port=%d ticket_len=%d upid=%q",
		node, vmid, raw.User, port, len(raw.Ticket), raw.UPID)
	return &TermProxyTicket{
		Ticket: raw.Ticket,
		Port:   port,
		UPID:   raw.UPID,
		User:   raw.User,
	}, nil
}

// DialConsoleWS opens the WebSocket to the VM's serial-console stream on
// the PVE node. Caller is responsible for closing the returned conn.
//
// Auth: API token in the Authorization header (works on PVE 7+). The
// vncticket query param is the per-session token from TermProxy and is
// consumed by PVE to bind to the right qemu socket.
//
// Subprotocol "binary" — PVE's vncwebsocket sends a base64-encoded text
// frame for the initial auth handshake then switches to raw bytes; the
// gorilla client handles the subprotocol negotiation, the relay handles
// the auth handshake.
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
		return nil, fmt.Errorf("dial console ws %s (status=%d): %w", u.String(), status, err)
	}
	return conn, nil
}

// ConsoleAuthHandshake completes PVE's in-band auth dance with the
// pve-xtermjs server reachable through the vncwebsocket bridge. PVE's
// API server proxies WS bytes to a local TCP socket where pve-xtermjs
// reads the first line as "<user>:<ticket>\n" and replies "OK\n" on
// success.
//
// Frame type matters here for API-token auth: tested both Text and
// Binary frames against PVE 8.x with API-token authentication; both
// produce a 1006 close. Logs the User and ticket prefix on send so a
// next-look at the journal can confirm what was actually transmitted,
// since the canonical wire format (`user:ticket\n`) only works when
// `user` matches what the termproxy call was authenticated as — and
// for API tokens PVE returns the full token id (`user@realm!tokenid`)
// which pve-xtermjs may or may not accept depending on PVE version.
func ConsoleAuthHandshake(conn *websocket.Conn, user, ticket string) error {
	payload := user + ":" + ticket + "\n"
	tPrefix := ticket
	if len(tPrefix) > 12 {
		tPrefix = tPrefix[:12] + "..."
	}
	log.Printf("proxmox console: sending auth handshake user=%q ticket_prefix=%q bytes=%d",
		user, tPrefix, len(payload))
	if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
		return fmt.Errorf("send auth: %w", err)
	}
	mt, msg, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("read auth ack: %w", err)
	}
	log.Printf("proxmox console: auth reply mt=%d bytes=%d body=%q", mt, len(msg), string(msg))
	if !strings.HasPrefix(string(msg), "OK") {
		return fmt.Errorf("console auth rejected: %q", strings.TrimSpace(string(msg)))
	}
	return nil
}
