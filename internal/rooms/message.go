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
//
// `ref` is the sender's own bookkeeping and only ever travels inbound: it is held
// raw so a wrong JSON type does not make the whole frame unreadable (that would
// look like invalid_frame and lose the type the refusal should name), and it is
// validated separately by refOf. It is never copied onto anything the server sends
// to anyone but the sender.
type envelope struct {
	Type string          `json:"t"`
	Data json.RawMessage `json:"d,omitempty"`
	From string          `json:"from,omitempty"`
	Ref  json.RawMessage `json:"ref,omitempty"`
}

// maxFrameBytes bounds a single relayed message. Generous for game state,
// nowhere near enough to use a room as a file transfer.
const maxFrameBytes = 64 << 10 // 64 KiB

// maxRefBytes bounds a request's `ref`. It is a correlation token, not a payload.
const maxRefBytes = 64

// refOf reads a frame's ref. Absent, null and "" all mean no ref. A ref that is
// not a string, or is longer than maxRefBytes, is refused (ok false) so the caller
// can answer invalid_ref and not act on the frame.
func refOf(e *envelope) (ref string, ok bool) {
	if len(e.Ref) == 0 || string(e.Ref) == "null" {
		return "", true
	}
	if err := json.Unmarshal(e.Ref, &ref); err != nil {
		return "", false
	}
	if len(ref) > maxRefBytes {
		return "", false
	}
	return ref, true
}

// refusal builds a `room:error` for a frame the sender sent: the code, the frame's
// own type verbatim, and the ref when it had a valid one, so a client can tell
// which of its requests was turned down. The key is left out when there is no ref
// rather than sent empty.
func refusal(code string, e *envelope, ref string) []byte {
	return mustControl(TypeError, answerData(code, e, ref))
}

// answer builds a `room:ok`: the host request in `e` was done.
func answer(e *envelope, ref string) []byte {
	return mustControl(TypeOK, answerData("", e, ref))
}

func answerData(code string, e *envelope, ref string) map[string]any {
	data := map[string]any{"type": e.Type}
	if code != "" {
		data["code"] = code
	}
	if ref != "" {
		data["ref"] = ref
	}
	return data
}

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
// preserving the payload byte-for-byte. The frame is built from the type, payload
// and sender only, so a ref the sender attached is never shown to anyone else.
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
