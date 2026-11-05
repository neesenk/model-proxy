package admin

import (
	"context"
	"errors"
	"testing"
	"time"

	"model-proxy/internal/appapi"
	"model-proxy/internal/configedit"
)

func TestEditConfigProviderReloadRetainsConfigLock(t *testing.T) {
	configFile := writeTestConfig(t)
	reloadStarted := make(chan struct{})
	releaseReload := make(chan struct{})
	service := New(Ports{
		ConfigFile: func() string { return configFile },
		HomeDir:    func() string { return t.TempDir() },
		Reload: func(string) error {
			close(reloadStarted)
			<-releaseReload
			return errors.New("reload failed")
		},
	})

	editDone := make(chan error, 1)
	go func() {
		editDone <- service.EditConfig(appapi.EditRequest{
			Kind: "provider",
			Name: "zhipu",
			Data: map[string]any{"billing": "pay-as-you-go"},
		})
	}()
	<-reloadStarted

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	lockErr := configedit.WithConfigLockContext(ctx, configFile, func() error {
		return errors.New("config lock acquired during provider reload")
	})
	cancel()
	close(releaseReload)
	editErr := <-editDone

	if !errors.Is(lockErr, context.DeadlineExceeded) {
		t.Errorf("lock probe during reload = %v, want context deadline while the mutation owns the lock", lockErr)
	}
	if editErr == nil || editErr.Error() != "reload failed" {
		t.Errorf("edit error = %v, want reload failed", editErr)
	}
}
