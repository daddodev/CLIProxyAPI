// Package usageupdates signals changes to the usage dashboard without carrying
// credentials or blocking request processing on slow dashboard clients.
package usageupdates

import "sync"

var subscribers = struct {
	sync.Mutex
	channels map[chan struct{}]struct{}
}{channels: make(map[chan struct{}]struct{})}

// Subscribe coalesces changes until the consumer reads the current state.
// The caller must release the subscription when its stream ends.
func Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	subscribers.Lock()
	subscribers.channels[ch] = struct{}{}
	subscribers.Unlock()
	return ch, func() {
		subscribers.Lock()
		delete(subscribers.channels, ch)
		subscribers.Unlock()
	}
}

// Notify never waits for a subscriber to read a notification.
func Notify() {
	subscribers.Lock()
	defer subscribers.Unlock()
	for ch := range subscribers.channels {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
