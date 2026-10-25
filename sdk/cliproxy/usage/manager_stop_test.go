package usage

import (
	"context"
	"sync/atomic"
	"testing"
)

type countingPlugin struct{ n atomic.Int64 }

func (p *countingPlugin) HandleUsage(context.Context, Record) { p.n.Add(1) }

func TestStopWaitsForQueuedRecords(t *testing.T) {
	m := NewManager(0)
	plugin := &countingPlugin{}
	m.Register(plugin)
	m.Start(context.Background())
	for i := 0; i < 500; i++ {
		m.Publish(context.Background(), Record{Provider: "claude", Model: "m"})
	}
	m.Stop()
	if got := plugin.n.Load(); got != 500 {
		t.Fatalf("delivered %d records before Stop returned, want 500", got)
	}
}
