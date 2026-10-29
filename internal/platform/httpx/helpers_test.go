package httpx

import (
	"strings"
	"testing"
)

func TestContentDispositionAttachmentUsesRFC5987(t *testing.T) {
	got := ContentDispositionAttachment("báo cáo @ 10:30.txt")
	want := `attachment; filename="b_o c_o @ 10:30.txt"; filename*=UTF-8''b%C3%A1o%20c%C3%A1o%20%40%2010%3A30.txt`
	if got != want {
		t.Fatalf("ContentDispositionAttachment() = %q, want %q", got, want)
	}
}

func TestContentDispositionAttachmentStripsPathAndControls(t *testing.T) {
	got := ContentDispositionAttachment("../a\r\nb.txt")
	want := `attachment; filename="ab.txt"; filename*=UTF-8''a%0D%0Ab.txt`
	if got != want {
		t.Fatalf("ContentDispositionAttachment() = %q, want %q", got, want)
	}
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("header contains a raw CR/LF: %q", got)
	}
}

func TestContentDispositionAttachmentUsesFallbackForDotOnlyNames(t *testing.T) {
	for _, name := range []string{"", ".", "..", "...", " . ", "folder/"} {
		got := ContentDispositionAttachment(name)
		want := `attachment; filename="download"; filename*=UTF-8''download`
		if got != want {
			t.Errorf("ContentDispositionAttachment(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestContentDispositionAttachmentEscapesQuotes(t *testing.T) {
	got := ContentDispositionAttachment(`quote"name.txt`)
	want := `attachment; filename="quote_name.txt"; filename*=UTF-8''quote%22name.txt`
	if got != want {
		t.Fatalf("ContentDispositionAttachment() = %q, want %q", got, want)
	}
}
