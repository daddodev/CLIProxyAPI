package usageupdates

import "testing"

func TestSlowSubscribersDoNotBlockOrLoseLatestInvalidation(t *testing.T) {
	first, unsubscribeFirst := Subscribe()
	defer unsubscribeFirst()
	second, unsubscribeSecond := Subscribe()
	for range 1000 {
		Notify()
	}
	for _, ch := range []<-chan struct{}{first, second} {
		select {
		case <-ch:
		default:
			t.Fatal("subscriber missed an invalidation")
		}
		select {
		case <-ch:
			t.Fatal("burst was not coalesced")
		default:
		}
	}
	unsubscribeSecond()
	unsubscribeSecond()
	Notify()
	select {
	case <-second:
		t.Fatal("unsubscribed stream still receives updates")
	default:
	}
	select {
	case <-first:
	default:
		t.Fatal("remaining subscriber lost updates")
	}
}
