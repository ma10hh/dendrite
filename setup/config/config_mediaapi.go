package config

import (
	"fmt"
	"net/netip"
	"time"
)

type MediaAPI struct {
	Matrix *Global `yaml:"-"`

	// The MediaAPI database stores information about files uploaded and downloaded
	// by local users. It is only accessed by the MediaAPI.
	Database DatabaseOptions `yaml:"database,omitempty"`

	// The base path to where the media files will be stored. May be relative or absolute.
	BasePath Path `yaml:"base_path"`

	// The absolute base path to where media files will be stored.
	AbsBasePath Path `yaml:"-"`

	// The maximum file size in bytes that is allowed to be stored on this server.
	// Note: if max_file_size_bytes is set to 0, the size is unlimited.
	// Note: if max_file_size_bytes is not set, it will default to 10485760 (10MB)
	MaxFileSizeBytes FileSizeBytes `yaml:"max_file_size_bytes,omitempty"`

	// Whether to dynamically generate thumbnails on-the-fly if the requested resolution is not already generated
	DynamicThumbnails bool `yaml:"dynamic_thumbnails"`

	// The maximum number of simultaneous thumbnail generators. default: 10
	MaxThumbnailGenerators int `yaml:"max_thumbnail_generators"`

	// A list of thumbnail sizes to be pre-generated for downloaded remote / uploaded content
	ThumbnailSizes []ThumbnailSize `yaml:"thumbnail_sizes"`

	// URLPreviews configures GET /preview_url, which makes this server fetch pages for its users.
	URLPreviews URLPreviews `yaml:"url_previews"`

	// MaxUserStorageBytes is how much media one local user may have stored at the same time. 0 is unlimited.
	MaxUserStorageBytes FileSizeBytes `yaml:"max_user_storage_bytes"`

	// Retention deletes media that nobody has asked for in a while.
	Retention MediaRetention `yaml:"retention"`
}

// MediaRetention configures the removal of media that has not been used for some time. A lifetime counts from the last
// download, or from the upload if there was none. 0 keeps the media for ever.
type MediaRetention struct {
	// LocalMediaLifetime is for media that users of this server uploaded. It cannot be fetched again once it is gone.
	LocalMediaLifetime time.Duration `yaml:"local_media_lifetime"`
	// RemoteMediaLifetime is for copies of media of other servers, which are fetched again when they are asked for.
	RemoteMediaLifetime time.Duration `yaml:"remote_media_lifetime"`
	// Interval is the time between two runs of the removal.
	Interval time.Duration `yaml:"interval"`
}

// URLPreviews configures the URL previews of the media API.
type URLPreviews struct {
	// Enabled turns the endpoint on. It is off by default: it makes the server connect to addresses that users name.
	Enabled bool `yaml:"enabled"`
	// MaxPageSizeBytes is how much of a page is read to find its title, description and image.
	MaxPageSizeBytes FileSizeBytes `yaml:"max_page_size_bytes"`
	// Timeout is the time one page or image may take, including redirects.
	Timeout time.Duration `yaml:"timeout"`
	// CacheLifetime is how long a preview is kept.
	CacheLifetime time.Duration `yaml:"cache_lifetime"`
	// MaxConcurrent is how many previews are made at the same time.
	MaxConcurrent int `yaml:"max_concurrent"`
	// IPRangeDenylist is denied in addition to the private, loopback, link local and reserved ranges, which are
	// always denied.
	IPRangeDenylist []string `yaml:"ip_range_denylist"`
	// IPRangeAllowlist lifts the denial for ranges that hold servers that are safe to preview, for example a
	// server inside the network.
	IPRangeAllowlist []string `yaml:"ip_range_allowlist"`
}

// DefaultMaxFileSizeBytes defines the default file size allowed in transfers
var DefaultMaxFileSizeBytes = FileSizeBytes(10485760)

func (c *MediaAPI) Defaults(opts DefaultOpts) {
	c.MaxFileSizeBytes = DefaultMaxFileSizeBytes
	c.MaxThumbnailGenerators = 10
	c.URLPreviews.MaxPageSizeBytes = 4 << 20
	c.URLPreviews.Timeout = 15 * time.Second
	c.URLPreviews.CacheLifetime = time.Hour
	c.URLPreviews.MaxConcurrent = 10
	c.Retention.Interval = time.Hour
	if opts.Generate {
		c.ThumbnailSizes = []ThumbnailSize{
			{
				Width:        32,
				Height:       32,
				ResizeMethod: "crop",
			},
			{
				Width:        96,
				Height:       96,
				ResizeMethod: "crop",
			},
			{
				Width:        640,
				Height:       480,
				ResizeMethod: "scale",
			},
		}
		if !opts.SingleDatabase {
			c.Database.ConnectionString = "file:mediaapi.db"
		}
		c.BasePath = "./media_store"
	}
}

func (c *MediaAPI) Verify(configErrs *ConfigErrors) {
	checkNotEmpty(configErrs, "media_api.base_path", string(c.BasePath))
	checkPositive(configErrs, "media_api.max_file_size_bytes", int64(c.MaxFileSizeBytes))
	checkPositive(configErrs, "media_api.max_thumbnail_generators", int64(c.MaxThumbnailGenerators))

	for i, size := range c.ThumbnailSizes {
		checkPositive(configErrs, fmt.Sprintf("media_api.thumbnail_sizes[%d].width", i), int64(size.Width))
		checkPositive(configErrs, fmt.Sprintf("media_api.thumbnail_sizes[%d].height", i), int64(size.Height))
	}

	checkPositive(configErrs, "media_api.max_user_storage_bytes", int64(c.MaxUserStorageBytes))
	checkPositive(configErrs, "media_api.retention.local_media_lifetime", int64(c.Retention.LocalMediaLifetime))
	checkPositive(configErrs, "media_api.retention.remote_media_lifetime", int64(c.Retention.RemoteMediaLifetime))
	if c.Retention.LocalMediaLifetime > 0 || c.Retention.RemoteMediaLifetime > 0 {
		if c.Retention.Interval <= 0 {
			configErrs.Add("invalid value for config key \"media_api.retention.interval\": must be above 0 when a lifetime is set")
		}
	}

	if c.URLPreviews.Enabled {
		checkPositive(configErrs, "media_api.url_previews.max_page_size_bytes", int64(c.URLPreviews.MaxPageSizeBytes))
		checkPositive(configErrs, "media_api.url_previews.timeout", int64(c.URLPreviews.Timeout))
		checkPositive(configErrs, "media_api.url_previews.cache_lifetime", int64(c.URLPreviews.CacheLifetime))
		checkPositive(configErrs, "media_api.url_previews.max_concurrent", int64(c.URLPreviews.MaxConcurrent))
		for name, ranges := range map[string][]string{
			"media_api.url_previews.ip_range_denylist":  c.URLPreviews.IPRangeDenylist,
			"media_api.url_previews.ip_range_allowlist": c.URLPreviews.IPRangeAllowlist,
		} {
			for _, r := range ranges {
				if _, err := netip.ParsePrefix(r); err != nil {
					if _, aerr := netip.ParseAddr(r); aerr != nil {
						configErrs.Add(fmt.Sprintf("%s: %q is not an IP address or a CIDR range", name, r))
					}
				}
			}
		}
	}

	if c.Matrix.DatabaseOptions.ConnectionString == "" {
		checkNotEmpty(configErrs, "media_api.database.connection_string", string(c.Database.ConnectionString))
	}
}
