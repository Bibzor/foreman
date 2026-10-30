package acceptance_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Bibzor/foreman/internal/acceptance"
)

func TestRedact(t *testing.T) {
	in := "step 3: token ghs_AbCdEfGhIjKlMnOpQrStUvWx0123 used\n" +
		"Authorization: Basic eC1hY2Nlc3MtdG9rZW46Z2hz\n" +
		"pat github_pat_11ABCDEFG0123456789_abcdefghij\n" +
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEow\n-----END RSA PRIVATE KEY-----\nafter"
	out := acceptance.Redact(in)
	for _, leaked := range []string{"ghs_AbCd", "eC1hY2Nl", "github_pat_11", "MIIEow"} {
		if strings.Contains(out, leaked) {
			t.Errorf("Redact left %q:\n%s", leaked, out)
		}
	}
	for _, kept := range []string{"step 3: token [redacted] used", "Authorization: [redacted]", "after"} {
		if !strings.Contains(out, kept) {
			t.Errorf("Redact lost %q:\n%s", kept, out)
		}
	}
}

func TestTail(t *testing.T) {
	var b strings.Builder
	for i := range 100 {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	got := acceptance.Tail(b.String())
	if lines := strings.Split(got, "\n"); len(lines) != 60 || lines[0] != "line 40" || lines[59] != "line 99" {
		t.Fatalf("Tail = %q", got)
	}
	if got := acceptance.Tail(strings.Repeat("x", 5000)); len(got) != 3000 {
		t.Fatalf("Tail kept %d bytes", len(got))
	}
}
