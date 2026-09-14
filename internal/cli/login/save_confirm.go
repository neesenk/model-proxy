package login

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"model-proxy/internal/accounts"
	"model-proxy/internal/display"
	logincore "model-proxy/internal/login"
)

// save_confirm.go holds the two steps every credential-login flow shares:
// the pre-lock replace confirmation and the post-save confirmation line.

// confirmReplace resolves the replace confirmation BEFORE the cross-process
// lock (stdin must never block the lock): a read-only LoadPool scan for id
// decides whether to prompt; if the user declines, the flow aborts without
// acquiring the lock. replace == true (flag) skips the prompt.
func confirmReplace(provName, providerID, id string, replace bool) error {
	if replace {
		return nil
	}
	existing, err := logincore.LoadPool(provName, providerID)
	if err != nil {
		return fmt.Errorf("load pool: %w", err)
	}
	for _, a := range existing.Accounts {
		if a.ID != id {
			continue
		}
		fmt.Printf("Account %q is already logged in. Replace its key? [y/N] ", a.Label)
		reader := bufio.NewReader(os.Stdin)
		ans, _ := reader.ReadString('\n')
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ans)), "y") {
			return fmt.Errorf("login cancelled")
		}
		break
	}
	return nil
}

// printSaved prints the confirmation line; the label is resolved from the
// freshly-saved pool, which may have been re-sorted by the pool save.
func printSaved(provName, providerID, id string) {
	pool, _ := logincore.LoadPool(provName, providerID)
	fmt.Println(display.Green("✓ Saved account ") + display.Gray(accounts.Mask(id)+" ("+logincore.AccountLabel(pool, id)+")"))
}
