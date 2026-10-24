package handler

import (
	"strings"
	"testing"
)

func TestContentDispositionAttachment(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{name: "plain", in: "report.pdf", want: []string{`filename="report.pdf"`, "filename*=UTF-8''report.pdf"}},
		{name: "path components", in: "../../etc/passwd", want: []string{`filename="passwd"`, "filename*=UTF-8''passwd"}},
		{name: "quotes", in: `a"b:c.txt`, want: []string{`filename="a_b:c.txt"`}},
		{name: "path traversal", in: `..\..\a.txt`, want: []string{`filename="a.txt"`}},
		{name: "unicode", in: "báo cáo.pdf", want: []string{`filename="b_o c_o.pdf"`, "filename*=UTF-8''b%C3%A1o%20c%C3%A1o.pdf"}},
		{name: "crlf injection", in: "a\r\nX-Evil: 1.txt", want: []string{`filename="aX-Evil: 1.txt"`}},
		{name: "empty", in: "", want: []string{`filename="download"`, "filename*=UTF-8''"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := contentDispositionAttachment(tt.in)
			if !strings.HasPrefix(got, "attachment; ") {
				t.Fatalf("missing attachment prefix: %q", got)
			}
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("contentDispositionAttachment(%q) = %q, want substring %q", tt.in, got, want)
				}
			}
			if strings.ContainsAny(got, "\r\n") {
				t.Errorf("header must not contain CR/LF: %q", got)
			}
		})
	}
}
