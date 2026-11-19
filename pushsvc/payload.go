package pushsvc

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	// PayloadVersion is data.v of the notification; the service worker ignores a version it does not know.
	PayloadVersion = 1
	// PayloadBudget is the ceiling of one payload, far under webpush.MaxPlaintext.
	PayloadBudget = 1024

	webPushMagic = 8030
	payloadLang  = "pt-BR"
)

// Job is the event of a push-sender Lambda: the notification already rendered, and who gets it. The sender reads the
// devices of each recipient and sends; it decides nothing about content.
type Job struct {
	EventID    string   `json:"event_id"`
	Type       string   `json:"type"`
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Navigate   string   `json:"navigate"`
	Tag        string   `json:"tag"`
	Topic      string   `json:"topic"`
	TTLSeconds int      `json:"ttl_seconds"`
	Urgency    string   `json:"urgency"`
	Recipients []string `json:"recipients"`
}

type data struct {
	V    int    `json:"v"`
	ID   string `json:"id"`
	Type string `json:"type"`
}

type notification struct {
	Title    string `json:"title"`
	Body     string `json:"body"`
	Navigate string `json:"navigate"`
	Tag      string `json:"tag"`
	Lang     string `json:"lang"`
	Silent   bool   `json:"silent"`
	Data     data   `json:"data"`
}

type envelope struct {
	WebPush      int          `json:"web_push"`
	Notification notification `json:"notification"`
}

// BuildPayload renders the notification of a job (the Declarative Web Push shape of contract v1,
// webpush/testdata/notification-v1.json) and refuses one over PayloadBudget or whose link is not a path of the app.
func BuildPayload(j Job) ([]byte, error) {
	if !strings.HasPrefix(j.Navigate, "/") || strings.HasPrefix(j.Navigate, "//") || strings.Contains(j.Navigate, "\\") {
		return nil, fmt.Errorf("pushsvc: navigate must be a path of the app")
	}
	body, err := json.Marshal(envelope{WebPush: webPushMagic, Notification: notification{
		Title: j.Title, Body: j.Body, Navigate: j.Navigate, Tag: j.Tag, Lang: payloadLang,
		Data: data{V: PayloadVersion, ID: j.EventID, Type: j.Type},
	}})
	if err != nil {
		return nil, err
	}
	if len(body) > PayloadBudget {
		return nil, fmt.Errorf("pushsvc: payload is %d bytes, over the %d budget", len(body), PayloadBudget)
	}
	return body, nil
}
