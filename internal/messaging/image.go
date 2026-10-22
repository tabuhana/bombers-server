package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"github.com/oklog/ulid/v2"

	"github.com/tabuhana/bombers-server/internal/auth"
	"github.com/tabuhana/bombers-server/internal/httpx"
	"github.com/tabuhana/bombers-server/internal/imagesize"
	"github.com/tabuhana/bombers-server/internal/logx"
)

// ObjectStore is the slice of the media store that pictures need. It is declared here
// and main hands in the media store, the way the console takes its own narrow view of
// it: messaging does not import media, and a domain reaching into another domain is the
// thing this codebase does not do. The keys are ours (imageKey) and the store is dumb
// bytes; what a picture is, who may see it and how long it lives are decided here.
type ObjectStore interface {
	PutObject(ctx context.Context, key string, data []byte, contentType string) error
	GetObject(ctx context.Context, key string) (io.ReadCloser, error)
	// RemovePrefix is how a picture is removed. It is an object under a prefix of its
	// own, so removing the prefix is removing the object, and the filesystem backend
	// takes the then-empty directory with it.
	RemovePrefix(ctx context.Context, prefix string) error
}

// ImagePrefix and imageKey are where a message's picture lives: under the message's OWN
// id, and nowhere else. That is the whole lifetime rule made structural. The bytes
// have no name but the message's, so nothing can share them between two messages, no
// request can point a message at somebody else's picture, and every way a message
// ends knows where to look: a delete removes them (Delete), and deleting an account,
// which cascades its messages away in the database where no handler sees it, has the
// console remove them (ImagePrefix is exported for that and nothing else). The
// trailing slash on the prefix matters: without it, removing one id would also take
// every other object whose key merely begins with the same characters.
func ImagePrefix(messageID string) string { return "messages/" + messageID + "/" }
func imageKey(messageID string) string    { return ImagePrefix(messageID) + "image" }

// SendImage is the other way to send: a picture, as a multipart/form-data body, to the
// friend named in ?to=. It is POST /messages for a message that is an image, and it
// takes the same trouble over WHO may be sent to: the same friendship gate with the
// same opaque 404 (sendTarget), the same nonce idempotency (taken from ?nonce=, because
// addressing is not content), and the same write under the seq lock (storeOnce). Only
// what is being sent differs, and that is read here from bytes the server has itself
// looked at.
//
// THE RULE still holds: a client does not declare a kind. Nothing about the message is
// the client's word - not that it is an image, not its type, not its size, not its
// dimensions. The type is sniffed, the dimensions are read out of the header
// (imagesize), the size is the length of what was read, and the kind is this handler's.
// A body that cannot be measured is refused, never stored without its size: a kind whose
// payload is sometimes incomplete pushes that problem into every client that draws it.
//
// The words under the picture are the message's BODY, which a client has always been
// the one to supply, and they go through the one rule a text body goes through
// (cleanCaption, which is cleanBody): they travel in the same request as the bytes so
// that there is one message, one row and one frame, not a picture and a caption that
// arrives a beat later. A picture with no words has an empty body.
//
// The order, and why:
//
//  1. the friendship gate, before the body is read, so a stranger cannot make the
//     server buffer ten megabytes;
//  2. read the parts under the caps (readImageUpload), clean the caption, sniff and
//     measure the file;
//  3. put the bytes;
//  4. insert the row.
//
// Bytes before the row, because the two ways to fail are not equal: a row whose bytes
// are missing is a broken picture a person can see, and an object whose row never
// landed is waste nobody can. So when the insert fails the bytes are removed - or kept,
// if it cannot be said that the row did not land (discardUnlessStored) - and when the
// removal fails too that is logged and left: dead weight, not a correctness problem.
func (h *Handler) SendImage(w http.ResponseWriter, r *http.Request) {
	senderID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	recipientID := strings.TrimSpace(queryParam(r, "to"))
	nonce := strings.TrimSpace(queryParam(r, "nonce"))
	if len([]rune(nonce)) > maxNonce {
		httpx.WriteError(w, http.StatusBadRequest, errNonceTooLong)
		return
	}
	if !h.sendTarget(w, r, senderID, recipientID) {
		return
	}
	if h.objects == nil {
		// Never in a running server: main always passes the media store.
		logx.Error("messaging: no object store, cannot store a picture")
		httpx.WriteError(w, http.StatusInternalServerError, "could not send image")
		return
	}

	upload, ok := readImageUpload(w, r)
	if !ok {
		return
	}
	data := upload.data

	// The declared Content-Type of the part is ignored: a mislabeled or malicious file
	// can't smuggle a non-image in, and a picture can't claim to be a type it is not.
	mime := http.DetectContentType(data)
	if !imagesize.Accepts(mime) {
		httpx.WriteError(w, http.StatusUnsupportedMediaType, errUnsupportedType)
		return
	}
	width, height, err := imagesize.Dimensions(mime, data)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidImage)
		return
	}
	payload := imagePayload{Width: width, Height: height, Mime: mime, Size: len(data)}
	if err := payload.check(); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidImage)
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		logx.Error("messaging: image payload: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not send image")
		return
	}

	id := ulid.Make().String()
	if err := h.objects.PutObject(r.Context(), imageKey(id), data, mime); err != nil {
		logx.Error("messaging: put image: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not send image")
		return
	}

	saved, created, err := h.storeOnce(r.Context(), &messageRecord{
		ID:          id,
		SenderID:    senderID,
		RecipientID: recipientID,
		Body:        upload.caption,
		Nonce:       nonce,
		Kind:        kindImage,
		Payload:     raw,
	})
	if err != nil {
		logx.Error("messaging: insert image: %v", err)
		h.discardUnlessStored(r.Context(), id)
		httpx.WriteError(w, http.StatusInternalServerError, "could not send image")
		return
	}
	if !created {
		// A retry of an upload that already landed: the picture under that nonce is
		// the one stored, and the bytes just put belong to a message that was never
		// made. Without this every retried upload would leave one behind.
		h.removeImage(r.Context(), id)
	}
	h.answerSend(w, r, senderID, saved, created, nonce)
}

// The parts of a picture upload. They are the whole vocabulary: any other part is a
// refusal, which is how a client naming a `kind` here is told no as it is on
// POST /messages.
const (
	imageFormFile    = "file"
	imageFormCaption = "caption"

	// maxCaptionBytes is how much of a caption part is read: what a whole text send is
	// allowed to be, so a caption is bounded the way a body is. It is a bound on memory
	// and not the rule for the words - that is cleanBody's 4000 characters.
	maxCaptionBytes = sendBodyLimit
	// maxImageRequestBytes bounds the request as a whole: the largest picture, the
	// largest caption, and room for the boundary lines and part headers around them.
	// A request that goes past it is not read further, whatever its parts claim.
	maxImageRequestBytes = maxImageBytes + maxCaptionBytes + 16<<10
)

// imageUpload is what a picture request holds once it has been read: the bytes of the
// file, which are not yet known to be a picture, and the caption as it will be stored.
type imageUpload struct {
	data    []byte
	caption string
}

// readImageUpload reads the multipart body of POST /messages/image. It writes the
// refusal itself, as sendTarget does, and reports whether the send may go on.
//
// The parts are read one at a time off the stream (multipart.Reader, never
// ParseMultipartForm, which would spool a part to a temporary file on disk), under
// three caps: the request as a whole, the file part at maxImageBytes and the caption
// part at maxCaptionBytes. Nothing bigger than those is ever held. The parts may come
// in either order; `file` and `caption` may each appear once, and any other part is
// refused unread. The raw part is taken (NextRawPart): the bytes the server sniffs are
// the bytes that were sent, not what a Content-Transfer-Encoding header asked to be
// undone.
//
// Every refusal is one this endpoint or POST /messages already had: 413
// message_too_large for anything past a cap, 400 invalid_body for a body that is not
// multipart, is cut short, has no file or has a part this does not take, and 400
// body_too_long for a caption past the rule a text body is held to. Nothing a client
// sends is a 500. What depends on what the parts say is checked after all of them are
// read, in a fixed order (structure, then the caption, then, in SendImage, the file),
// so the answer does not depend on which part came first.
func readImageUpload(w http.ResponseWriter, r *http.Request) (up imageUpload, ok bool) {
	// multipart/form-data and nothing else: http.Request.MultipartReader would take
	// multipart/mixed too, which is not what this endpoint is documented to read.
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidBody)
		return up, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImageRequestBytes)
	mr := multipart.NewReader(r.Body, params["boundary"])

	var rawCaption []byte
	var haveFile, haveCaption, captionTooLong bool
	for {
		part, err := mr.NextRawPart()
		// Compared, not errors.Is: a body that ends without its closing boundary comes
		// back as a WRAPPED io.EOF, which is a cut-off body and not the end of the parts.
		if err == io.EOF {
			break
		}
		if err != nil {
			refuseUnreadable(w, err)
			return up, false
		}
		switch part.FormName() {
		case imageFormFile:
			if haveFile {
				httpx.WriteError(w, http.StatusBadRequest, errInvalidBody)
				return up, false
			}
			haveFile = true
			// One byte past the cap, to tell a picture of exactly maxImageBytes from one
			// that is larger.
			if up.data, err = io.ReadAll(io.LimitReader(part, maxImageBytes+1)); err != nil {
				refuseUnreadable(w, err)
				return up, false
			}
			if len(up.data) > maxImageBytes {
				httpx.WriteError(w, http.StatusRequestEntityTooLarge, errMessageTooLarge)
				return up, false
			}
		case imageFormCaption:
			if haveCaption {
				httpx.WriteError(w, http.StatusBadRequest, errInvalidBody)
				return up, false
			}
			haveCaption = true
			if rawCaption, err = io.ReadAll(io.LimitReader(part, maxCaptionBytes+1)); err != nil {
				refuseUnreadable(w, err)
				return up, false
			}
			captionTooLong = len(rawCaption) > maxCaptionBytes
		default:
			httpx.WriteError(w, http.StatusBadRequest, errInvalidBody)
			return up, false
		}
	}
	if !haveFile {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidBody)
		return up, false
	}

	if captionTooLong {
		httpx.WriteError(w, http.StatusBadRequest, errBodyTooLong)
		return up, false
	}
	caption, code := cleanCaption(string(rawCaption))
	if code != "" {
		httpx.WriteError(w, http.StatusBadRequest, code)
		return up, false
	}
	up.caption = caption
	return up, true
}

// refuseUnreadable answers a request body that could not be read to the end: past the
// request cap is the same 413 as a picture past its own, and anything else - not
// multipart, a part cut short, a header that is not one - is a body that was not valid.
func refuseUnreadable(w http.ResponseWriter, err error) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		httpx.WriteError(w, http.StatusRequestEntityTooLarge, errMessageTooLarge)
		return
	}
	httpx.WriteError(w, http.StatusBadRequest, errInvalidBody)
}

// ServeImage streams a message's picture back, through the server, with the caller's
// own Authorization header - never a bucket URL and never a presigned link, so every
// fetch is checked and the bucket stays private (the rule profile media follows).
//
// Only the message's two people, and only while they are still friends, which is the
// rule for reading anything else in the thread: history, the catch-up feed and search
// all omit a conversation with somebody who is no longer a friend, and a picture that
// stayed readable would be a way to keep reading it. Everything else - a third party,
// an unknown id, a deleted message, a message that is not an image, a former friend -
// is the same opaque 404 message_not_found, so an id cannot be probed for.
//
// The object is looked for only after all of that, and under the key made from the
// stored row's id, never from the request.
func (h *Handler) ServeImage(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	id := strings.TrimSpace(urlParam(r, "messageID"))
	msg, ok := h.loadParticipant(w, r, id, authedID, true, "could not load image")
	if !ok {
		return
	}
	if msg.Kind != kindImage {
		httpx.WriteError(w, http.StatusNotFound, errMessageNotFound)
		return
	}
	var p imagePayload
	if !decodeStrict(msg.Payload, &p) || p.check() != nil {
		// A live image row whose payload is not an image's cannot have been written by
		// this code. Loud in the log; the client sees what it sees for any other id.
		logx.Error("messaging: image message %s has a payload that is not an image's", msg.ID)
		httpx.WriteError(w, http.StatusNotFound, errMessageNotFound)
		return
	}

	// The bytes of a message never change (its key is its id and nothing rewrites it),
	// so the id is the whole version. The conditional request is answered after the
	// authorization above, so a 304 tells a caller nothing they could not already read.
	etag := `"` + msg.ID + `"`
	if r.Header.Get("If-None-Match") == etag {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	obj, err := h.objects.GetObject(r.Context(), imageKey(msg.ID))
	if err != nil {
		// Not a 404: the caller is allowed to see this picture, so there is nothing to
		// hide, and a 404 would say the message is gone when the store is only briefly
		// unreachable. A row whose bytes are missing is a fault worth being loud about.
		logx.Error("messaging: get image %s: %v", msg.ID, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not load image")
		return
	}
	defer obj.Close()

	w.Header().Set("Content-Type", p.Mime)
	w.Header().Set("Content-Length", strconv.Itoa(p.Size))
	w.Header().Set("ETag", etag)
	// The type was sniffed from these bytes at upload, and stays what it was sniffed
	// as: a browser must not look again and decide this is something else.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Not cached by the webview, for the reason profile media is not: the app keeps its
	// own copy on disk, which is what it counts and what it can clear.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, obj); err != nil {
		// Headers are flushed; nothing to send the client. Usually a canceled download.
		logx.Error("messaging: stream image %s: %v", msg.ID, err)
	}
}

// removeImage removes a message's picture, best-effort: a failure is logged and
// nothing more. Both callers have already committed what mattered. After a delete the
// row is a tombstone and the person has done what they asked, and refusing it because a
// bucket was briefly unreachable would be refusing the one thing they asked for; the
// cost is bytes nothing can reach, which is waste and not a fault. After a failed or
// repeated send the object has no message at all.
//
// It runs on a context that outlives the request: the client hanging up the instant
// it has been answered must not leave the bytes behind.
func (h *Handler) removeImage(ctx context.Context, messageID string) {
	if h.objects == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pushTimeout)
	defer cancel()
	if err := h.objects.RemovePrefix(ctx, ImagePrefix(messageID)); err != nil {
		logx.Error("messaging: remove image %s (left behind): %v", messageID, err)
	}
}

// discardUnlessStored removes the bytes of a picture whose insert returned an error -
// unless the row may be there. An error from the commit does not say the commit failed:
// the connection can be lost after the database has accepted it, and removing the
// bytes then would leave a message whose picture is gone, the failure the order of
// operations exists to avoid. So the row is asked for, and the bytes are removed only
// when it is known not to exist. If it cannot be asked, they stay: an object nobody
// points to is invisible waste, and a picture that points at nothing is not.
func (h *Handler) discardUnlessStored(ctx context.Context, messageID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pushTimeout)
	defer cancel()
	stored, err := messageExists(ctx, h.db, messageID)
	if err != nil {
		logx.Error("messaging: cannot tell whether image message %s was stored, keeping its bytes: %v", messageID, err)
		return
	}
	if stored {
		return
	}
	h.removeImage(ctx, messageID)
}
