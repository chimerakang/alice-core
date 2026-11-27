package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
		allowed, err := readCodexQuota(ctx, s.Runner.CodexScoutBinary)
		if err == nil && allowed != nil {
			if *allowed {
				if cleared, err := s.Store.clearQuotaObservedBefore(ctx, "codex", observedBefore); err == nil && cleared {
					s.Kick()
				}
			} else {
				_ = s.Store.recordCodexQuotaDenied(ctx, observedBefore)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// readCodexQuota uses the App Server's account read with the CLI's managed
// login. Only an explicit ordinaryUsageAllowed=true confirms recovery.
func readCodexQuota(ctx context.Context, binary string) (*bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "app-server")
	cmd.Dir = os.TempDir()
	cmd.Env = client.CLIEnv()
	cmd.WaitDelay = 2 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	defer stdout.Close()
	if err := cmd.Start(); err != nil {
		return nil, err
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
		return nil, err
	}
	if err := enc.Encode(map[string]string{"method": "initialized"}); err != nil {
		return nil, err
	}
	var quota struct {
		OrdinaryUsageAllowed *bool `json:"ordinaryUsageAllowed"`
	}
	if err := request(2, "account/rateLimits/read", map[string]bool{"excludeResetCreditDetails": true}, &quota); err != nil {
		return nil, err
	}
	return quota.OrdinaryUsageAllowed, nil
}
