package pushsvc

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The golden file is go-common's own contract fixture (webpush/testdata): the push-client service worker is tested
// against the same bytes, so a drift here is a contract change (bump PayloadVersion).
func TestBuildPayloadMatchesGolden(t *testing.T) {
	raw, err := os.ReadFile("../webpush/testdata/notification-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := BuildPayload(Job{
		EventID: "01JEXEMPLOEVENTID0000000000", Type: "document.rejected", Title: "NF-e rejeitada · Empresa Exemplo",
		Body: "Toque para ver o motivo.", Tag: "document.rejected:a1b2c3d4",
		Navigate: "/nfe/detail?key=35260112345678000190550010000000011000000011&org=ORG_EXEMPLO",
	})
	if err != nil {
		t.Fatal(err)
	}
	var want, have map[string]any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &have); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, have) {
		t.Errorf("payload drifted from the golden contract:\n got  %s\n want %s", got, raw)
	}
}

func TestBuildPayloadRejectsNavigate(t *testing.T) {
	for _, nav := range []string{"x", "", "//evil.example", "/a\\b", "https://evil.example/"} {
		if _, err := BuildPayload(Job{Title: "t", Navigate: nav}); err == nil {
			t.Errorf("navigate %q accepted", nav)
		}
	}
}

func TestBuildPayloadRejectsOverBudget(t *testing.T) {
	_, err := BuildPayload(Job{Title: "t", Body: strings.Repeat("x", PayloadBudget), Navigate: "/"})
	if err == nil {
		t.Error("a payload over the budget was accepted")
	}
}
