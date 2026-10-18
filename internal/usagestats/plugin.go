package usagestats

import (
	"context"
	"sync/atomic"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

var activeTracker atomic.Pointer[Tracker]

func init() {
	coreusage.RegisterPlugin(&usagePlugin{})
}

// SetActive sets the tracker that receives usage records. nil disables recording.
func SetActive(t *Tracker) {
	activeTracker.Store(t)
}

// Active returns the tracker that receives usage records, or nil when disabled.
func Active() *Tracker {
	return activeTracker.Load()
}

type usagePlugin struct{}

// HandleUsage forwards each usage record to the active tracker.
func (p *usagePlugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if p == nil {
		return
	}
	if t := Active(); t != nil {
		t.Record(ctx, record)
	}
}
