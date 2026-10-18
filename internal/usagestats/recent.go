package usagestats

import "time"

const (
	// recentRequestTTL bounds how long a request id keeps suppressing a second request count.
	recentRequestTTL = 15 * time.Minute
	// recentRequestMax bounds the number of request ids remembered at once.
	recentRequestMax = 50000
)

// recentRequestSet remembers recently counted request ids. Entries expire after
// ttl and the oldest entries are evicted once max is reached, so memory stays
// bounded under load. It is not safe for concurrent use; the aggregator guards it.
type recentRequestSet struct {
	ttl   time.Duration
	max   int
	seen  map[string]time.Time
	queue []recentRequestEntry
	head  int
}

type recentRequestEntry struct {
	key string
	at  time.Time
}

func newRecentRequestSet(ttl time.Duration, max int) *recentRequestSet {
	return &recentRequestSet{
		ttl:  ttl,
		max:  max,
		seen: make(map[string]time.Time),
	}
}

// add records key as seen at now. It reports false when key was already seen
// within the ttl, meaning the request must not be counted again.
func (s *recentRequestSet) add(key string, now time.Time) bool {
	s.expire(now)
	if _, ok := s.seen[key]; ok {
		return false
	}
	for len(s.seen) >= s.max && s.head < len(s.queue) {
		s.popFront()
	}
	s.seen[key] = now
	s.queue = append(s.queue, recentRequestEntry{key: key, at: now})
	return true
}

func (s *recentRequestSet) reset() {
	s.seen = make(map[string]time.Time)
	s.queue = nil
	s.head = 0
}

func (s *recentRequestSet) expire(now time.Time) {
	for s.head < len(s.queue) && now.Sub(s.queue[s.head].at) > s.ttl {
		s.popFront()
	}
}

func (s *recentRequestSet) popFront() {
	entry := s.queue[s.head]
	if at, ok := s.seen[entry.key]; ok && at.Equal(entry.at) {
		delete(s.seen, entry.key)
	}
	s.queue[s.head] = recentRequestEntry{}
	s.head++
	// Compact the backing slice once the consumed prefix dominates it.
	if s.head >= 1024 && s.head*2 >= len(s.queue) {
		s.queue = append([]recentRequestEntry(nil), s.queue[s.head:]...)
		s.head = 0
	}
}
