package barkmonitor

import (
	"context"
	"testing"
)

func TestSayRejectsRequestsItCannotSpeak(t *testing.T) {
	b := &barkMonitor{}
	for name, cmd := range map[string]map[string]interface{}{
		"no text":    {"command": "say"},
		"no speaker": {"command": "say", "text": "home mode"},
	} {
		if _, err := b.DoCommand(context.Background(), cmd); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
