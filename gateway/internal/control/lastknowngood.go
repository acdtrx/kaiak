package control

import (
	"encoding/json"

	"kaiak/internal/config"
	"kaiak/internal/schemacheck"
)

// LastKnownGoodFile is the data-directory file holding the last config applied from
// the control plane, the gateway's config at boot when the control plane is
// unreachable (docs/specs/GATEWAY.md, Configuration sources).
const LastKnownGoodFile = "last-known-good.json"

// lastKnownGoodFormat is the file's format version; a file with another is discarded.
const lastKnownGoodFormat = 5

// lastKnownGood is the file's data: the config document as the control plane sent it,
// its version and the epoch the version counts in.
type lastKnownGood struct {
	ConfigEpoch string          `json:"config_epoch"`
	Version     int64           `json:"version"`
	Config      json.RawMessage `json:"config"`
}

// saveLastKnownGood writes the config just applied, when there is a data directory.
// A failed write is logged: the running config is unaffected, only the next
// unreachable boot loses its fallback.
func (c *Client) saveLastKnownGood(at configPosition, doc []byte) {
	if c.opts.Dir == nil {
		return
	}
	if err := c.opts.Dir.WriteVersioned(LastKnownGoodFile, lastKnownGoodFormat,
		lastKnownGood{ConfigEpoch: at.epoch, Version: at.version, Config: doc}); err != nil {
		c.logger.Error("last-known-good config not written", "file", LastKnownGoodFile,
			"config_version", at.version, "config_epoch", at.epoch, "error", err)
		return
	}
	c.logger.Debug("last-known-good config written", "file", LastKnownGoodFile, "config_version", at.version,
		"config_epoch", at.epoch)
}

// bootFromLastKnownGood applies the last-known-good copy, if there is a data directory
// and a copy in it, through the
// shared apply path; it reports whether a config is in force. The copy is not written
// back. Applied, the stream resumes after its version unless a snapshot was already
// taken; rejected, the client fetches the snapshot first. A rejection of the snapshot
// fetched just before stays reported: the copy is not a config the control plane sent
// now, so it answers nothing about that one.
func (c *Client) bootFromLastKnownGood() bool {
	if c.opts.Dir == nil {
		return false
	}
	var saved lastKnownGood
	found, err := c.opts.Dir.ReadVersioned(LastKnownGoodFile, lastKnownGoodFormat, &saved)
	if err != nil {
		c.logger.Warn("last-known-good config not read", "file", LastKnownGoodFile, "error", err)
		return false
	}
	if !found {
		c.logger.Info("no last-known-good config", "file", LastKnownGoodFile)
		return false
	}
	if !hex32Pattern.MatchString(saved.ConfigEpoch) || saved.Version < 1 || saved.Version > schemacheck.MaxSafeInteger {
		// The stream would resume from this position, and the control plane refuses a
		// malformed one on every reconnect.
		c.logger.Warn("last-known-good config discarded: malformed position", "file", LastKnownGoodFile,
			"config_version", saved.Version, "config_epoch", saved.ConfigEpoch)
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.opts.Applier.ApplyPublished(TriggerLastKnownGood, saved.Config,
		config.Version{Epoch: saved.ConfigEpoch, Number: saved.Version},
		"config_version", saved.Version, "config_epoch", saved.ConfigEpoch, "file", LastKnownGoodFile)
	if err != nil {
		return false
	}
	if c.position.version == 0 {
		c.position = configPosition{epoch: saved.ConfigEpoch, version: saved.Version}
	}
	c.applied = &configPosition{epoch: saved.ConfigEpoch, version: saved.Version}
	return true
}
