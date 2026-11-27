package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"time"

	"github.com/chimerakang/alice-core/client"
)

const codexQuotaPollInterval = 5 * time.Minute

func (s *Scheduler) refreshCodexQuota(ctx context.Context) {
	ticker := time.NewTicker(codexQuotaPollInterval)
	defer ticker.Stop()
	for {
		observedBefore := s.Store.now()
		snapshot, err := readCodexQuota(ctx, s.Runner.CodexScoutBinary, observedBefore)
		if err == nil {
			if !snapshot.resetAt.IsZero() {
				_ = s.Store.recordProviderQuotaMeasurement(ctx, "codex", snapshot, observedBefore)
			}
			if snapshot.allowed != nil {
				if *snapshot.allowed {
					if cleared, err := s.Store.clearQuotaObservedBefore(ctx, "codex", observedBefore); err == nil && cleared {
						s.Kick()
					}
				} else {
					_ = s.Store.recordCodexQuotaDenied(ctx, observedBefore, snapshot.resetAt)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type providerQuotaSnapshot struct {
	allowed   *bool
	remaining float64
	resetAt   time.Time
}

type codexQuotaWindow struct {
	UsedPercent *float64 `json:"usedPercent"`
	ResetsAt    int64    `json:"resetsAt"`
}

type codexQuotaBucket struct {
	Primary, Secondary *codexQuotaWindow
}

type codexQuotaReply struct {
	OrdinaryUsageAllowed *bool                       `json:"ordinaryUsageAllowed"`
	RateLimits           codexQuotaBucket            `json:"rateLimits"`
	RateLimitsByLimitID  map[string]codexQuotaBucket `json:"rateLimitsByLimitId"`
}

// snapshot uses the most constraining active window in the ordinary Codex
// bucket. When both windows are exhausted, both must reset before admission.
func (q codexQuotaReply) snapshot(now time.Time) providerQuotaSnapshot {
	bucket := q.RateLimits
	if primary, ok := q.RateLimitsByLimitID["codex"]; ok {
		bucket = primary
	}
	out := providerQuotaSnapshot{allowed: q.OrdinaryUsageAllowed}
	for _, window := range []*codexQuotaWindow{bucket.Primary, bucket.Secondary} {
		if window == nil || window.UsedPercent == nil {
			continue
		}
		used := *window.UsedPercent
		reset := time.Unix(window.ResetsAt, 0).UTC()
		if used < 0 || used > 100 || math.IsNaN(used) || !reset.After(now) {
			continue
		}
		remaining := 1 - used/100
		if out.resetAt.IsZero() || remaining < out.remaining || (remaining == 0 && out.remaining == 0 && reset.After(out.resetAt)) {
			out.remaining, out.resetAt = remaining, reset
		}
	}
	if !out.resetAt.IsZero() {
		allowed := out.remaining > 0
		if out.allowed == nil || !allowed {
			out.allowed = &allowed
		}
	}
	return out
}

// readCodexQuota queries the public App Server protocol.
func readCodexQuota(ctx context.Context, binary string, now time.Time) (providerQuotaSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "app-server")
	cmd.Dir = os.TempDir()
	cmd.Env = client.CLIEnv()
	cmd.WaitDelay = 2 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return providerQuotaSnapshot{}, err
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return providerQuotaSnapshot{}, err
	}
	defer stdout.Close()
	if err := cmd.Start(); err != nil {
		return providerQuotaSnapshot{}, err
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	enc := json.NewEncoder(stdin)
	dec := json.NewDecoder(io.LimitReader(stdout, 4<<20))

	request := func(id int, method string, params any, result any) error {
		if err := enc.Encode(map[string]any{"id": id, "method": method, "params": params}); err != nil {
			return err
		}
		for {
			var reply struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if err := dec.Decode(&reply); err != nil {
				return err
			}
			if reply.ID != id {
				continue
			}
			if len(reply.Error) != 0 && string(reply.Error) != "null" {
				return errors.New("Codex App Server rejected " + method)
			}
			if result != nil {
				return json.Unmarshal(reply.Result, result)
			}
			return nil
		}
	}

	if err := request(1, "initialize", map[string]any{
		"clientInfo": map[string]string{"name": "alice_fleet", "version": "1"},
	}, nil); err != nil {
		return providerQuotaSnapshot{}, err
	}
	if err := enc.Encode(map[string]string{"method": "initialized"}); err != nil {
		return providerQuotaSnapshot{}, err
	}
	var quota codexQuotaReply
	if err := request(2, "account/rateLimits/read", nil, &quota); err != nil {
		return providerQuotaSnapshot{}, err
	}
	return quota.snapshot(now), nil
}
