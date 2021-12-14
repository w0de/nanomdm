package microwebhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

func postWebhookEvent(
	ctx context.Context,
	client *http.Client,
	url string,
	event *Event,
) error {
	jsonBytes, err := json.MarshalIndent(event, "", "\t")
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(jsonBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf(
			"unexpected HTTP status %d %s for webhook, url: %s, event: %s, topic: %s, enrollment id: %s, command_uuid: %s",
			resp.StatusCode, resp.Status, url, event.EventID, event.EnrollmentID, event.CommandUUID
		)
	}
	return nil
}
