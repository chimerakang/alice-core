package fleet

import "time"

const EventQuotaExhausted = "quota.exhausted"

// QuotaExhaustion is a provider rejection. A zero ResetsAt means the
// provider gave no usable reset timestamp.
type QuotaExhaustion struct {
	Backend  string    `json:"backend"`
	ResetsAt time.Time `json:"resets_at"`
}
