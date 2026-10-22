package media

import (
	"os"
	"strings"
	"testing"
)

// Upload needs a database to run, so what it checks the sniffed type against is pinned
// as text: the accept-list is imagesize's, the same one message images use, and there
// is not a second one here to drift from it.
func TestUploadAcceptsTheSharedPictureList(t *testing.T) {
	raw, err := os.ReadFile("handler.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	if !strings.Contains(src, "if !imagesize.Accepts(contentType) {") {
		t.Error("Upload does not check the sniffed type with imagesize.Accepts")
	}
	if strings.Contains(src, "allowedContentTypes") {
		t.Error("media has its own accept-list again")
	}
}
