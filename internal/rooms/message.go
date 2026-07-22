package rooms

import "encoding/json"

// The wire envelope. Deliberately minimal: a type string the server routes on
// only for its own `room:` control messages, and an opaque payload it NEVER
// looks inside. `from` is stamped by the server so a client can't spoof another
// member (the field is ignored on the way in).
//
// This shape is what lets one relay serve both cadences: a chess move every
// thirty seconds and a position update thirty times a second are the same
// envelope with different `t` values and different rates.
type envelope struct {
	Type string          `json:"t"`
	Data json.RawMessage `json:"d,omitempty"`
	From string          `json:"from,omitempty"`
}

// maxFrameBytes bounds a single relayed message. Generous for game state,
// nowhere near enough to use a room as a file transfer.
const maxFrameBytes = 64 << 10 // 64 KiB

// decodeIncoming parses a client frame. A frame without a type is refused: the
// server can't stamp and forward something it can't identify, and silently
// dropping it would look like the network eating messages.
func decodeIncoming(raw []byte) (*envelope, bool) {
	var e envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, false
	}
	if e.Type == "" {
		return nil, false
	}
	return &e, true
}

// stampFrom re-emits a client frame with the authenticated sender attached,
// preserving the payload byte-for-byte.
func stampFrom(e *envelope, userID string) ([]byte, error) {
	return json.Marshal(envelope{Type: e.Type, Data: e.Data, From: userID})
}

// encodeControl builds one of the server's own `room:` messages.
func encodeControl(msgType string, data map[string]any) ([]byte, error) {
	payload, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{Type: msgType, Data: payload})
}
