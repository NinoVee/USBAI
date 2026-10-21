package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ninovee/usbai/internal/config"
	"github.com/ninovee/usbai/internal/vault"
)

// Portals are Apps-tab entries for services that run elsewhere, such as an
// Automaton in Conway Cloud. Nothing is installed or run on this computer:
// the portal opens the service's pages and, with the user's API key (kept
// encrypted in the vault), shows the account's status.

// conwayAPI is a variable so tests can point it at a local server.
var conwayAPI = "https://api.conway.tech"

// PortalInfo is what the Apps tab shows for a portal.
type PortalInfo struct {
	Links    []config.AppLink `json:"links"`
	KeySaved bool             `json:"key_saved"`
	Status   *PortalStatus    `json:"status,omitempty"`
	Error    string           `json:"error,omitempty"`
}

// PortalStatus is a Conway account's credits and cloud machines.
type PortalStatus struct {
	CreditsUSD float64         `json:"credits_usd"`
	Machines   []PortalMachine `json:"machines"`
	Checked    time.Time       `json:"checked"`
}

// PortalMachine is one Conway Cloud sandbox.
type PortalMachine struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	Region      string `json:"region,omitempty"`
	VCPU        int    `json:"vcpu,omitempty"`
	MemoryMB    int    `json:"memory_mb,omitempty"`
	TerminalURL string `json:"terminal_url,omitempty"`
	Created     string `json:"created_at,omitempty"`
}

func (a *App) portalApp(id string) (config.App, error) {
	for _, c := range a.cfg.Apps {
		if c.ID == id && c.Kind == "portal" {
			return c, nil
		}
	}
	return config.App{}, fmt.Errorf("no portal %q on this drive", id)
}

// appKeys are API keys for portals, by app id, kept in the vault.
func (a *App) appKeys() (map[string]string, error) {
	keys := map[string]string{}
	v, err := a.unlocked()
	if err != nil {
		return nil, err
	}
	if err := v.GetJSON("app_keys", &keys); err != nil && !errors.Is(err, vault.ErrNotFound) {
		return nil, err
	}
	return keys, nil
}

// SavePortalKey stores a portal's API key; "-" removes it.
func (a *App) SavePortalKey(id, key string) error {
	if _, err := a.portalApp(id); err != nil {
		return err
	}
	keys, err := a.appKeys()
	if err != nil {
		return err
	}
	switch key = strings.TrimSpace(key); key {
	case "":
		return errors.New("paste the API key")
	case "-":
		delete(keys, id)
	default:
		keys[id] = key
	}
	v, err := a.unlocked()
	if err != nil {
		return err
	}
	return v.PutJSON("app_keys", keys)
}

// Portal returns a portal's links and, when a key is saved, its status.
func (a *App) Portal(ctx context.Context, id string) (PortalInfo, error) {
	c, err := a.portalApp(id)
	if err != nil {
		return PortalInfo{}, err
	}
	keys, err := a.appKeys()
	if err != nil {
		return PortalInfo{}, err
	}
	info := PortalInfo{Links: safeLinks(c.Links), KeySaved: keys[id] != ""}
	if info.KeySaved && c.Status == "conway" {
		st, err := conwayStatus(ctx, keys[id])
		if err != nil {
			info.Error = err.Error()
		} else {
			info.Status = st
		}
	}
	return info, nil
}

// safeLinks keeps only https links, so a page can't be given a script URL.
func safeLinks(links []config.AppLink) []config.AppLink {
	var out []config.AppLink
	for _, l := range links {
		if httpsURL(l.URL) {
			out = append(out, l)
		}
	}
	return out
}

func httpsURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != ""
}

// conwayStatus reads the account's credit balance and cloud machines.
func conwayStatus(ctx context.Context, key string) (*PortalStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	get := func(path string, out any) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, conwayAPI+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", key) // Conway takes the key as-is
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("can't reach Conway Cloud: %w", err)
		}
		defer resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return errors.New("Conway rejected the API key — check it")
		case resp.StatusCode != http.StatusOK:
			return fmt.Errorf("Conway Cloud: %s", resp.Status)
		}
		return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
	}
	var bal struct {
		BalanceCents *float64 `json:"balance_cents"`
		CreditsCents *float64 `json:"credits_cents"`
	}
	if err := get("/v1/credits/balance", &bal); err != nil {
		return nil, err
	}
	st := &PortalStatus{Checked: time.Now()}
	if bal.BalanceCents != nil {
		st.CreditsUSD = *bal.BalanceCents / 100
	} else if bal.CreditsCents != nil {
		st.CreditsUSD = *bal.CreditsCents / 100
	}
	var raw json.RawMessage
	if err := get("/v1/sandboxes", &raw); err != nil {
		return nil, err
	}
	// The list comes bare or as {"sandboxes": [...]}.
	type box struct {
		ID          string `json:"id"`
		SandboxID   string `json:"sandbox_id"`
		Status      string `json:"status"`
		Region      string `json:"region"`
		VCPU        int    `json:"vcpu"`
		MemoryMB    int    `json:"memory_mb"`
		TerminalURL string `json:"terminal_url"`
		CreatedAt   string `json:"created_at"`
	}
	var boxes []box
	if json.Unmarshal(raw, &boxes) != nil {
		var wrapped struct {
			Sandboxes []box `json:"sandboxes"`
		}
		json.Unmarshal(raw, &wrapped)
		boxes = wrapped.Sandboxes
	}
	for _, b := range boxes {
		m := PortalMachine{ID: b.ID, Status: b.Status, Region: b.Region, VCPU: b.VCPU, MemoryMB: b.MemoryMB, Created: b.CreatedAt}
		if m.ID == "" {
			m.ID = b.SandboxID
		}
		if m.Status == "" {
			m.Status = "unknown"
		}
		if httpsURL(b.TerminalURL) {
			m.TerminalURL = b.TerminalURL
		}
		st.Machines = append(st.Machines, m)
	}
	return st, nil
}
