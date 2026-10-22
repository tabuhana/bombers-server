package transfers

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/tabuhana/bombers-server/internal/auth"
	"github.com/tabuhana/bombers-server/internal/httpx"
)

// DefaultSTUN is what GET /ice answers with when the operator has configured
// nothing: the same public STUN servers the client carried before the server
// handed out its own list, so a server that never sets ICE_STUN_URLS changes
// nothing for anyone.
var DefaultSTUN = []string{
	"stun:stun.l.google.com:19302",
	"stun:stun1.l.google.com:19302",
}

// iceServer is one entry of the list a browser's RTCPeerConnection takes. It has
// no username or credential fields, and that is the point: they exist only for
// TURN, and there is no TURN here.
type iceServer struct {
	URLs []string `json:"urls"`
}

type iceResponse struct {
	ICEServers []iceServer `json:"iceServers"`
}

// ICE answers with the STUN servers both ends of a transfer should use to find a
// route to each other.
//
// STUN only, and never TURN, for the same reason the bytes never cross this
// server: a TURN relay carries every byte of the transfer, which is exactly the
// metered bandwidth the peer-to-peer design exists to avoid. A pair of machines
// that cannot reach each other directly fail to connect and say so; they are not
// quietly relayed at the operator's expense. Serving the list from here instead
// of baking it into the client also means a self-hosted server on an island need
// not depend on anyone else's STUN.
//
// Behind authentication like everything else, so the server's own STUN choice is
// not an open directory. It depends on nothing the caller sent.
func (h *Handler) ICE(w http.ResponseWriter, r *http.Request) {
	if _, ok := auth.UserIDFromContext(r.Context()); !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, iceResponse{ICEServers: []iceServer{{URLs: h.ice}}})
}

// ParseSTUN reads the operator's ICE_STUN_URLS: a comma-separated list of
// `stun:host[:port]` or `stuns:host[:port]` URLs. Empty (or only blanks) means
// DefaultSTUN. Anything else — a malformed URL, a bad port, and above all a
// `turn:` or `turns:` URL — is an error, so a mistake stops the server from
// booting instead of quietly handing every client a list that cannot work or
// one that costs money.
func ParseSTUN(csv string) ([]string, error) {
	var out []string
	for _, raw := range strings.Split(csv, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		u, err := cleanSTUN(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	if len(out) == 0 {
		return append([]string(nil), DefaultSTUN...), nil
	}
	return out, nil
}

// cleanSTUN checks one URL and returns it with the scheme lower-cased.
func cleanSTUN(raw string) (string, error) {
	scheme, rest, ok := strings.Cut(raw, ":")
	if !ok {
		return "", fmt.Errorf("ice: %q is not a stun: URL", raw)
	}
	scheme = strings.ToLower(scheme)
	switch scheme {
	case "stun", "stuns":
	case "turn", "turns":
		return "", fmt.Errorf("ice: %q is a TURN server; Bombers relays no transfer bytes, so only stun: and stuns: are allowed", raw)
	default:
		return "", fmt.Errorf("ice: %q is not a stun: or stuns: URL", raw)
	}

	host, port := rest, ""
	if h, p, err := net.SplitHostPort(rest); err == nil {
		host, port = h, p
	} else if strings.HasPrefix(rest, "[") && strings.HasSuffix(rest, "]") {
		host = rest[1 : len(rest)-1]
	}
	if !validHost(host) {
		return "", fmt.Errorf("ice: %q has no usable host", raw)
	}
	if port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("ice: %q has a bad port", raw)
		}
	}
	return scheme + ":" + rest, nil
}

// validHost accepts an IP address or a DNS name. It is deliberately strict —
// letters, digits, dots and hyphens, not starting or ending in a dot or hyphen —
// because this string is handed to every client.
func validHost(h string) bool {
	if h == "" {
		return false
	}
	if net.ParseIP(h) != nil {
		return true
	}
	if strings.HasPrefix(h, ".") || strings.HasSuffix(h, ".") || strings.HasPrefix(h, "-") || strings.HasSuffix(h, "-") {
		return false
	}
	for _, c := range h {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '.', c == '-':
		default:
			return false
		}
	}
	return true
}
