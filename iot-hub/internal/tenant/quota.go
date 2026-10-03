package tenant

import (
	"fmt"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/grafana"
	"github.com/williamtatendajose/prediction/iot-hub/internal/ingest"
)

// Info is a tenant as the platform registry stores it.
type Info struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Status  string `json:"status"` // active | suspended
	Created int64  `json:"created"`
	Quota   Quota  `json:"quota"`
	// DeviceSelfService lets the tenant's admins issue device credentials.
	// Off: only the superadmin can (the platform hands out connection
	// strings).
	DeviceSelfService bool   `json:"deviceSelfService"`
	Note              string `json:"note,omitempty"`
	// Grafana is the tenant's Grafana organization, when provisioned.
	Grafana *grafana.Result `json:"grafana,omitempty"`
}

const (
	Active    = "active"
	Suspended = "suspended"
)

// Quota limits a tenant, like an IoT Hub tier. Zero means the platform
// default; a negative value means unlimited.
type Quota struct {
	MaxSensors          int     `json:"maxSensors,omitempty"`
	MaxDevices          int     `json:"maxDevices,omitempty"`
	MessagesPerSecond   float64 `json:"messagesPerSecond,omitempty"`
	MessagesPerDay      int64   `json:"messagesPerDay,omitempty"`
	RawRetentionDays    int     `json:"rawRetentionDays,omitempty"`
	RollupRetentionDays int     `json:"rollupRetentionDays,omitempty"`
	MaxJobs             int     `json:"maxJobs,omitempty"`
}

// Or fills zero fields from def.
func (q Quota) Or(def Quota) Quota {
	if q.MaxSensors == 0 {
		q.MaxSensors = def.MaxSensors
	}
	if q.MaxDevices == 0 {
		q.MaxDevices = def.MaxDevices
	}
	if q.MessagesPerSecond == 0 {
		q.MessagesPerSecond = def.MessagesPerSecond
	}
	if q.MessagesPerDay == 0 {
		q.MessagesPerDay = def.MessagesPerDay
	}
	if q.RawRetentionDays == 0 {
		q.RawRetentionDays = def.RawRetentionDays
	}
	if q.RollupRetentionDays == 0 {
		q.RollupRetentionDays = def.RollupRetentionDays
	}
	if q.MaxJobs == 0 {
		q.MaxJobs = def.MaxJobs
	}
	return q
}

func days(n int) time.Duration {
	if n <= 0 {
		return 0 // forever
	}
	return time.Duration(n) * 24 * time.Hour
}

// IDs are used in MQTT topics, directory names and Postgres schema names,
// so they are lowercase and short.
var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$`)

var reserved = map[string]bool{"admin": true, "api": true, "sys": true, "platform": true, "share": true}

func ValidID(id string) error {
	if !idRe.MatchString(id) || reserved[id] {
		return fmt.Errorf("tenant id must be 2-40 characters of a-z, 0-9 and '-', not reserved")
	}
	return nil
}

// Limiter enforces a message rate (token bucket, burst of 2 s) and a daily
// count (UTC days). One per tenant, so a noisy tenant can't starve others.
type Limiter struct {
	mu     sync.Mutex
	rate   float64 // per second; <= 0 unlimited
	perDay int64   // <= 0 unlimited
	tokens float64
	last   time.Time
	day    int
	count  int64
	now    func() time.Time

	Rejected atomic.Int64
	Total    atomic.Int64
}

func NewLimiter(rate float64, perDay int64) *Limiter {
	l := &Limiter{now: time.Now}
	l.Set(rate, perDay)
	return l
}

func (l *Limiter) Set(rate float64, perDay int64) {
	l.mu.Lock()
	l.rate, l.perDay = rate, perDay
	l.tokens = 2 * rate
	l.mu.Unlock()
}

func dayOf(t time.Time) int { y, m, d := t.UTC().Date(); return y*10000 + int(m)*100 + d }

// Admit takes one message or returns ingest.ErrQuota.
func (l *Limiter) Admit() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if d := dayOf(now); d != l.day {
		l.day, l.count = d, 0
	}
	if l.perDay > 0 && l.count >= l.perDay {
		l.Rejected.Add(1)
		return fmt.Errorf("%w: %d messages per day", ingest.ErrQuota, l.perDay)
	}
	if l.rate > 0 {
		if !l.last.IsZero() {
			l.tokens = min(2*l.rate, l.tokens+now.Sub(l.last).Seconds()*l.rate)
		}
		l.last = now
		if l.tokens < 1 {
			l.Rejected.Add(1)
			return fmt.Errorf("%w: %g messages per second", ingest.ErrQuota, l.rate)
		}
		l.tokens--
	}
	l.count++
	l.Total.Add(1)
	return nil
}

// Today is the number of messages admitted today (UTC).
func (l *Limiter) Today() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if dayOf(l.now()) != l.day {
		return 0
	}
	return l.count
}
