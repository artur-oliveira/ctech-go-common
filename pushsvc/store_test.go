package pushsvc

import (
	"fmt"
	"strings"
	"testing"
)

func TestSubscriptionStringRedacts(t *testing.T) {
	s := Subscription{ID: "abc", Endpoint: "https://fcm.googleapis.com/secret-endpoint", P256dh: "KEYKEYKEY", Auth: "AUTHAUTH"}
	for _, out := range []string{fmt.Sprintf("%v", s), fmt.Sprintf("%+v", s), fmt.Sprintf("%#v", s)} {
		for _, secret := range []string{"secret-endpoint", "KEYKEYKEY", "AUTHAUTH"} {
			if strings.Contains(out, secret) {
				t.Errorf("%q leaks %q", out, secret)
			}
		}
	}
}

func TestSubscriptionIDIs16HexChars(t *testing.T) {
	id := SubscriptionID("https://example.com/x")
	if len(id) != 16 || strings.Trim(id, "0123456789abcdef") != "" {
		t.Errorf("id = %q", id)
	}
}
