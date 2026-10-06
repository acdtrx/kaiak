package control

import (
	"encoding/json"
)

// LastKnownGoodFile is the data-directory file holding the last config applied from
// the control plane, the gateway's config at boot when the control plane is
// unreachable (docs/specs/GATEWAY.md, Configuration sources).
const LastKnownGoodFile = "last-known-good.json"

// lastKnownGoodFormat is the file's format version; a file with another is discarded.
const lastKnownGoodFormat = 7

// lastKnownGood is the file's data: the config document as the control plane sent it
// and its hash.
type lastKnownGood struct {
	ConfigHash string          `json:"config_hash"`
	Config     json.RawMessage `json:"config"`
}

// saveLastKnownGood writes the config just applied, when there is a data directory.
// A failed write is logged: the running config is unaffected, only the next
// unreachable boot loses its fallback.
func (c *Client) saveLastKnownGood(e ConfigEvent) {
	if c.opts.Dir == nil {
		return
	}
	if err := c.opts.Dir.WriteVersioned(LastKnownGoodFile, lastKnownGoodFormat,
		lastKnownGood{ConfigHash: e.ConfigHash, Config: e.Config}); err != nil {
		c.logger.Error("last-known-good config not written", "file.name", LastKnownGoodFile,
			"kaiak.config.hash", e.ConfigHash, "exception.message", err)
		return
	}
	c.logger.Debug("last-known-good config written", "file.name", LastKnownGoodFile, "kaiak.config.hash", e.ConfigHash)
}

// bootFromLastKnownGood applies the last-known-good copy, if there is a data directory
// and a copy in it, through the shared apply path; it reports whether a config is in
// force. The copy is not written back. Applied, its hash is the applied one, so the
// stream's config replaces it unless it is the same config. A rejection of the
// stream's config received just before stays reported: the copy is not a config the
// control plane sent now, so it answers nothing about that one.
func (c *Client) bootFromLastKnownGood() bool {
	if c.opts.Dir == nil {
		return false
	}
	var saved lastKnownGood
	found, err := c.opts.Dir.ReadVersioned(LastKnownGoodFile, lastKnownGoodFormat, &saved)
	if err != nil {
		c.logger.Warn("last-known-good config not read", "file.name", LastKnownGoodFile, "exception.message", err)
		return false
	}
	if !found {
		c.logger.Info("no last-known-good config", "file.name", LastKnownGoodFile)
		return false
	}
	if !configHashPattern.MatchString(saved.ConfigHash) {
		// Status would report it, and the schema refuses a malformed hash.
		c.logger.Warn("last-known-good config discarded: malformed config hash", "file.name", LastKnownGoodFile)
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.opts.Applier.Apply(TriggerLastKnownGood, saved.Config,
		"kaiak.config.hash", saved.ConfigHash, "file.name", LastKnownGoodFile)
	if err != nil {
		return false
	}
	c.appliedHash = saved.ConfigHash
	return true
}
