package config

import "time"

type SyncAPI struct {
	Matrix *Global `yaml:"-"`

	Database DatabaseOptions `yaml:"database,omitempty"`

	RealIPHeader string `yaml:"real_ip_header"`

	Fulltext Fulltext `yaml:"search"`

	// Retention deletes old messages, following the retention policies of rooms and of this server.
	Retention MessageRetention `yaml:"retention"`
}

// RetentionPolicy is how long messages of a room are kept. 0 means no limit.
type RetentionPolicy struct {
	// MinLifetime is how long a message is kept at least. It is what a room promises to its members.
	MinLifetime time.Duration `yaml:"min_lifetime"`
	// MaxLifetime is how long a message is kept at most. Older messages are deleted.
	MaxLifetime time.Duration `yaml:"max_lifetime"`
}

// MessageRetention configures the deletion of messages that are older than the retention policy of their room, the
// m.room.retention state event, allows. State events are never deleted.
type MessageRetention struct {
	// Enabled turns the deletion on. Off by default: it deletes messages for good.
	Enabled bool `yaml:"enabled"`
	// DefaultPolicy applies to rooms that have no m.room.retention event.
	DefaultPolicy RetentionPolicy `yaml:"default_policy"`
	// AllowedLifetimeMin raises the lifetimes of room policies that are shorter. 0 allows any.
	AllowedLifetimeMin time.Duration `yaml:"allowed_lifetime_min"`
	// AllowedLifetimeMax lowers the lifetimes of room policies that are longer. 0 allows any.
	AllowedLifetimeMax time.Duration `yaml:"allowed_lifetime_max"`
	// Interval is the time between two runs of the deletion.
	Interval time.Duration `yaml:"interval"`
}

func (c *SyncAPI) Defaults(opts DefaultOpts) {
	c.Fulltext.Defaults(opts)
	c.Retention.Interval = time.Hour
	if opts.Generate {
		if !opts.SingleDatabase {
			c.Database.ConnectionString = "file:syncapi.db"
		}
	}
}

func (c *SyncAPI) Verify(configErrs *ConfigErrors) {
	c.Fulltext.Verify(configErrs)
	c.Retention.Verify(configErrs)
	if c.Matrix.DatabaseOptions.ConnectionString == "" {
		checkNotEmpty(configErrs, "sync_api.database", string(c.Database.ConnectionString))
	}
}

func (r *MessageRetention) Verify(configErrs *ConfigErrors) {
	checkPositive(configErrs, "sync_api.retention.default_policy.min_lifetime", int64(r.DefaultPolicy.MinLifetime))
	checkPositive(configErrs, "sync_api.retention.default_policy.max_lifetime", int64(r.DefaultPolicy.MaxLifetime))
	checkPositive(configErrs, "sync_api.retention.allowed_lifetime_min", int64(r.AllowedLifetimeMin))
	checkPositive(configErrs, "sync_api.retention.allowed_lifetime_max", int64(r.AllowedLifetimeMax))
	if !r.Enabled {
		return
	}
	if r.Interval <= 0 {
		configErrs.Add("invalid value for config key \"sync_api.retention.interval\": must be above 0")
	}
	if r.AllowedLifetimeMin > 0 && r.AllowedLifetimeMax > 0 && r.AllowedLifetimeMin > r.AllowedLifetimeMax {
		configErrs.Add("invalid value for config key \"sync_api.retention.allowed_lifetime_min\": above allowed_lifetime_max")
	}
	if p := r.DefaultPolicy; p.MinLifetime > 0 && p.MaxLifetime > 0 && p.MinLifetime > p.MaxLifetime {
		configErrs.Add("invalid value for config key \"sync_api.retention.default_policy.min_lifetime\": above max_lifetime")
	}
}

type Fulltext struct {
	Enabled   bool   `yaml:"enabled"`
	IndexPath Path   `yaml:"index_path"`
	InMemory  bool   `yaml:"in_memory"` // only useful in tests
	Language  string `yaml:"language"`  // the language to use when analysing content
}

func (f *Fulltext) Defaults(opts DefaultOpts) {
	f.Enabled = false
	f.IndexPath = "./searchindex"
	f.Language = "en"
}

func (f *Fulltext) Verify(configErrs *ConfigErrors) {
	if !f.Enabled {
		return
	}
	checkNotEmpty(configErrs, "syncapi.search.index_path", string(f.IndexPath))
	checkNotEmpty(configErrs, "syncapi.search.language", f.Language)
}
