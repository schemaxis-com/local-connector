package manager

import (
	"regexp"
	"strings"
	"testing"
)

// TestEveryLabelNamesItsControl keeps the connection form usable with a
// screen reader: a label that neither points at a control (for="…") nor
// wraps one names nothing, so the field is announced without a name and
// clicking the text does not focus it.
func TestEveryLabelNamesItsControl(t *testing.T) {
	raw, err := WebAssets.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("reading the embedded page: %v", err)
	}
	page := string(raw)

	labelRE := regexp.MustCompile(`(?s)<label\b([^>]*)>(.*?)</label>`)
	forRE := regexp.MustCompile(`\bfor="([^"]+)"`)
	controlRE := regexp.MustCompile(`<(input|select|textarea)\b`)

	labels := labelRE.FindAllStringSubmatch(page, -1)
	if len(labels) < 5 {
		t.Fatalf("found %d labels; the pattern no longer matches the page", len(labels))
	}
	for _, label := range labels {
		attrs, body := label[1], label[2]
		if controlRE.MatchString(body) {
			continue
		}
		target := forRE.FindStringSubmatch(attrs)
		if target == nil {
			t.Errorf("label %q names no control", strings.TrimSpace(body))
			continue
		}
		if !strings.Contains(page, `id="`+target[1]+`"`) {
			t.Errorf("label %q points at missing id %q", strings.TrimSpace(body), target[1])
		}
	}
}
