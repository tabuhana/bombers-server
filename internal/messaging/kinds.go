package messaging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/oklog/ulid/v2"
)

// A message has a KIND. Today there are three, a closed set (the column has a CHECK
// that says so): text, which is everything a person types; invite, which says "come
// to my room"; and image, which is not written by anything yet.
//
// THE RULE: A CLIENT CANNOT DECLARE A KIND. POST /messages is text-only and refuses a
// request that names any other. If a client could post an invite with a room id in its
// payload, an invite would be a claim in a message again, which is the
// `bombers://room/<id>` shortcut that was deleted - a thing the server holds no state
// about and anyone can forge. The server MINTS the non-text kinds itself, from state it
// owns: an invite message is written when the invite is made (MintInvite), and an
// image message will be written by an endpoint that checks the media is the caller's.
const (
	kindText   = "text"
	kindInvite = "invite"
	kindImage  = "image"
)

var (
	// errUnknownKind is a kind outside the closed set. The column can never hold a
	// word nothing renders because nothing here writes one.
	errUnknownKind = errors.New("unknown message kind")
	// errKindNotMintable is a kind in the set that nothing writes yet.
	errKindNotMintable = errors.New("message kind cannot be written yet")
	// errBadPayload is a payload that is not the shape its kind has.
	errBadPayload = errors.New("payload does not fit the message kind")
)

// invitePayload is what an invite message carries. room_name is a COPY taken when
// the invite was made, so the message still reads sensibly after a rename or once
// the room is gone. The invite's live state is deliberately not here: the client
// reads that from the invite itself, so a card can never disagree with the truth.
type invitePayload struct {
	InviteID string `json:"invite_id"`
	Room     string `json:"room"`
	RoomName string `json:"room_name"`
}

// checkKindPayload is the one place that says what a kind may carry. The server
// does not interpret a payload beyond this: it refuses a kind it does not know, and a
// payload that is not the shape of the kind it is storing.
func checkKindPayload(kind string, payload []byte) error {
	switch kind {
	case kindText:
		if len(payload) != 0 {
			return fmt.Errorf("%w: a text message has no payload", errBadPayload)
		}
		return nil
	case kindInvite:
		dec := json.NewDecoder(bytes.NewReader(payload))
		dec.DisallowUnknownFields()
		var p invitePayload
		if err := dec.Decode(&p); err != nil || dec.More() {
			return fmt.Errorf("%w: invite", errBadPayload)
		}
		if p.InviteID == "" || p.Room == "" || p.RoomName == "" {
			return fmt.Errorf("%w: invite needs invite_id, room and room_name", errBadPayload)
		}
		return nil
	case kindImage:
		return errKindNotMintable
	}
	return errUnknownKind
}

// MintInvite writes the message that says "come to my room": from the inviter to the
// invitee, kind invite, with the invite's id, the room and the room's name as it is
// now. rooms calls it through an injected func (rooms may not import messaging, the
// same seam as the notify fan-out), once per NEW invite - never for a re-ask, which
// returns the invite that already exists.
//
// It is the only writer of an invite message, and it is why a client cannot be:
// everything in the payload came from the invite the room just made.
//
// The words of the message are empty. What the message says is the payload, and the
// client draws it; a sentence stored here would be English the server wrote on the
// user's behalf, indexed by search as if the user had typed it.
//
// The friendship is checked again here even though rooms has checked it: this
// package is the one place that knows who may be put in front of whom, and an
// invite a friend can't be sent a message about is an invite that should not
// exist. An error means no message was written; the caller must void the invite.
func (h *Handler) MintInvite(ctx context.Context, inviteID, roomID, roomName, senderID, recipientID string) error {
	roomName = plainText(roomName)
	if inviteID == "" || roomID == "" || roomName == "" || senderID == "" || recipientID == "" {
		return fmt.Errorf("mint invite message: %w", errBadPayload)
	}
	if code := h.guardRecipient(ctx, senderID, recipientID); code != "" {
		return errors.New("mint invite message: recipient is not a friend")
	}
	payload, err := json.Marshal(invitePayload{InviteID: inviteID, Room: roomID, RoomName: roomName})
	if err != nil {
		return fmt.Errorf("mint invite message: %w", err)
	}
	saved, err := writeSeq(ctx, h.begin, func(tx dbExecutor) (*messageRecord, error) {
		return insertMessage(ctx, tx, &messageRecord{
			ID:          ulid.Make().String(),
			SenderID:    senderID,
			RecipientID: recipientID,
			Kind:        kindInvite,
			Payload:     payload,
		})
	})
	if err != nil {
		return fmt.Errorf("mint invite message: %w", err)
	}
	// Stored, so it is safe to say so.
	h.pushDM(ctx, opNew, saved, "")
	return nil
}
