package config

import (
	"fmt"
	"strings"
	"time"
)

type ClientAPI struct {
	Matrix  *Global  `yaml:"-"`
	Derived *Derived `yaml:"-"` // TODO: Nuke Derived from orbit

	// If set disables new users from registering (except via shared
	// secrets)
	RegistrationDisabled bool `yaml:"registration_disabled"`

	// If set, requires users to submit a token during registration.
	// Tokens can be managed using admin API.
	RegistrationRequiresToken bool `yaml:"registration_requires_token"`

	// Enable registration without captcha verification or shared secret.
	// This option is populated by the -really-enable-open-registration
	// command line parameter as it is not recommended.
	OpenRegistrationWithoutVerificationEnabled bool `yaml:"-"`

	// If set, allows registration by anyone who also has the shared
	// secret, even if registration is otherwise disabled.
	RegistrationSharedSecret string `yaml:"registration_shared_secret"`
	// If set, prevents guest accounts from being created. Only takes
	// effect if registration is enabled, otherwise guests registration
	// is forbidden either way.
	GuestsDisabled bool `yaml:"guests_disabled"`

	// Boolean stating whether catpcha registration is enabled
	// and required
	RecaptchaEnabled bool `yaml:"enable_registration_captcha"`
	// Recaptcha api.js Url, for compatible with hcaptcha.com, etc.
	RecaptchaApiJsUrl string `yaml:"recaptcha_api_js_url"`
	// Recaptcha div class for sitekey, for compatible with hcaptcha.com, etc.
	RecaptchaSitekeyClass string `yaml:"recaptcha_sitekey_class"`
	// Recaptcha form field, for compatible with hcaptcha.com, etc.
	RecaptchaFormField string `yaml:"recaptcha_form_field"`
	// This Home Server's ReCAPTCHA public key.
	RecaptchaPublicKey string `yaml:"recaptcha_public_key"`
	// This Home Server's ReCAPTCHA private key.
	RecaptchaPrivateKey string `yaml:"recaptcha_private_key"`
	// Secret used to bypass the captcha registration entirely
	RecaptchaBypassSecret string `yaml:"recaptcha_bypass_secret"`
	// HTTP API endpoint used to verify whether the captcha response
	// was successful
	RecaptchaSiteVerifyAPI string `yaml:"recaptcha_siteverify_api"`

	// TURN options
	TURN TURN `yaml:"turn"`

	// Rate-limiting options
	RateLimiting RateLimiting `yaml:"rate_limiting"`

	// Delegate authentication to an external OAuth 2.0 / OpenID Connect provider (MSC3861).
	MSC3861 MSC3861 `yaml:"msc3861"`

	// Rendezvous sessions for signing in with a QR code (MSC4388).
	Rendezvous Rendezvous `yaml:"rendezvous"`

	// DelayedEvents lets users send events later, which Element Call uses (MSC4140).
	DelayedEvents DelayedEvents `yaml:"delayed_events"`

	// MatrixRTC says where clients find the media servers of calls (MSC4143).
	MatrixRTC MatrixRTC `yaml:"matrix_rtc"`

	// MaxToDeviceMessages is how many (user, device) targets one sendToDevice request may have. 0 means no limit.
	MaxToDeviceMessages int `yaml:"max_to_device_messages"`

	MSCs *MSCs `yaml:"-"`
}

func (c *ClientAPI) Defaults(opts DefaultOpts) {
	c.RegistrationSharedSecret = ""
	c.RegistrationRequiresToken = false
	c.RecaptchaPublicKey = ""
	c.RecaptchaPrivateKey = ""
	c.RecaptchaEnabled = false
	c.RecaptchaBypassSecret = ""
	c.RecaptchaSiteVerifyAPI = ""
	c.RegistrationDisabled = true
	c.OpenRegistrationWithoutVerificationEnabled = false
	c.RateLimiting.Defaults()
	c.DelayedEvents.MaxDelay = 24 * time.Hour
	c.DelayedEvents.MaxPerUser = 100
	c.MaxToDeviceMessages = 10000
}

func (c *ClientAPI) Verify(configErrs *ConfigErrors) {
	checkPositive(configErrs, "client_api.max_to_device_messages", int64(c.MaxToDeviceMessages))
	c.TURN.Verify(configErrs)
	c.RateLimiting.Verify(configErrs)
	c.MSC3861.Verify(configErrs)
	c.DelayedEvents.Verify(configErrs)
	c.MatrixRTC.Verify(configErrs)
	if c.RecaptchaEnabled {
		if c.RecaptchaSiteVerifyAPI == "" {
			c.RecaptchaSiteVerifyAPI = "https://www.google.com/recaptcha/api/siteverify"
		}
		if c.RecaptchaApiJsUrl == "" {
			c.RecaptchaApiJsUrl = "https://www.google.com/recaptcha/api.js"
		}
		if c.RecaptchaFormField == "" {
			c.RecaptchaFormField = "g-recaptcha-response"
		}
		if c.RecaptchaSitekeyClass == "" {
			c.RecaptchaSitekeyClass = "g-recaptcha"
		}
		checkNotEmpty(configErrs, "client_api.recaptcha_public_key", c.RecaptchaPublicKey)
		checkNotEmpty(configErrs, "client_api.recaptcha_private_key", c.RecaptchaPrivateKey)
		checkNotEmpty(configErrs, "client_api.recaptcha_siteverify_api", c.RecaptchaSiteVerifyAPI)
		checkNotEmpty(configErrs, "client_api.recaptcha_sitekey_class", c.RecaptchaSitekeyClass)
	}
	// Ensure there is any spam counter measure when enabling registration
	if !c.RegistrationDisabled && !c.OpenRegistrationWithoutVerificationEnabled && !c.RecaptchaEnabled {
		configErrs.Add(
			"You have tried to enable open registration without any secondary verification methods " +
				"(such as reCAPTCHA). By enabling open registration, you are SIGNIFICANTLY " +
				"increasing the risk that your server will be used to send spam or abuse, and may result in " +
				"your server being banned from some rooms. If you are ABSOLUTELY CERTAIN you want to do this, " +
				"start Dendrite with the -really-enable-open-registration command line flag. Otherwise, you " +
				"should set the registration_disabled option in your Dendrite config.",
		)
	}
}

type TURN struct {
	// TODO Guest Support
	// Whether or not guests can request TURN credentials
	// AllowGuests bool `yaml:"turn_allow_guests"`
	// How long the authorization should last
	UserLifetime string `yaml:"turn_user_lifetime"`
	// The list of TURN URIs to pass to clients
	URIs []string `yaml:"turn_uris"`

	// Authorization via Shared Secret
	// The shared secret from coturn
	SharedSecret string `yaml:"turn_shared_secret"`

	// Authorization via Static Username & Password
	// Hardcoded Username and Password
	Username string `yaml:"turn_username"`
	Password string `yaml:"turn_password"`
}

func (c *TURN) Verify(configErrs *ConfigErrors) {
	value := c.UserLifetime
	if value != "" {
		if _, err := time.ParseDuration(value); err != nil {
			configErrs.Add(fmt.Sprintf("invalid duration for config key %q: %s", "client_api.turn.turn_user_lifetime", value))
		}
	}
}

type RateLimiting struct {
	// Is rate limiting enabled or disabled?
	Enabled bool `yaml:"enabled"`

	// How many "slots" a user can occupy sending requests to a rate-limited
	// endpoint before we apply rate-limiting
	Threshold int64 `yaml:"threshold"`

	// The cooloff period in milliseconds after a request before the "slot"
	// is freed again
	CooloffMS int64 `yaml:"cooloff_ms"`

	// A list of users that are exempt from rate limiting, i.e. if you want
	// to run Mjolnir or other bots.
	ExemptUserIDs []string `yaml:"exempt_user_ids"`
}

func (r *RateLimiting) Verify(configErrs *ConfigErrors) {
	if r.Enabled {
		checkPositive(configErrs, "client_api.rate_limiting.threshold", r.Threshold)
		checkPositive(configErrs, "client_api.rate_limiting.cooloff_ms", r.CooloffMS)
	}
}

func (r *RateLimiting) Defaults() {
	r.Enabled = true
	r.Threshold = 5
	r.CooloffMS = 500
}

// Rendezvous configures the rendezvous sessions two devices use to sign in one device with
// the other by scanning a QR code (MSC4388).
type Rendezvous struct {
	Enabled bool `yaml:"enabled"`
	// If set anyone can create a rendezvous session, otherwise only users with an access token.
	// Open sessions let a new device use this server before it has an account on it, but the
	// server stores arbitrary short-lived data for anybody.
	AllowUnauthenticated bool `yaml:"allow_unauthenticated"`
}

// DelayedEvents configures the events that users ask to be sent later (MSC4140).
type DelayedEvents struct {
	// Enabled turns the feature on. Off by default: a client that sends a delay while it is off is refused, not
	// answered by sending the event at once.
	Enabled bool `yaml:"enabled"`
	// MaxDelay is the longest delay a user may ask for.
	MaxDelay time.Duration `yaml:"max_delay"`
	// MaxPerUser is how many delayed events one user may have waiting.
	MaxPerUser int `yaml:"max_per_user"`
}

func (d *DelayedEvents) Verify(configErrs *ConfigErrors) {
	if !d.Enabled {
		return
	}
	if d.MaxDelay <= 0 {
		configErrs.Add("invalid value for config key \"client_api.delayed_events.max_delay\": must be above 0")
	}
	if d.MaxPerUser <= 0 {
		configErrs.Add("invalid value for config key \"client_api.delayed_events.max_per_user\": must be above 0")
	}
}

// MatrixRTC is what clients need to find the media servers of calls (MSC4143).
type MatrixRTC struct {
	// Transports are the media servers that clients may use, in the order of preference.
	Transports []RTCTransport `yaml:"transports"`
}

// RTCTransport is one media server for calls.
type RTCTransport struct {
	// Type is "livekit".
	Type string `yaml:"type"`
	// LiveKitServiceURL is the URL of the service that hands out LiveKit tokens (lk-jwt-service).
	LiveKitServiceURL string `yaml:"livekit_service_url"`
}

func (m *MatrixRTC) Verify(configErrs *ConfigErrors) {
	for i, t := range m.Transports {
		key := fmt.Sprintf("client_api.matrix_rtc.transports[%d]", i)
		if t.Type != "livekit" {
			configErrs.Add(fmt.Sprintf("invalid value for config key %q: type must be livekit, got %q", key+".type", t.Type))
			continue
		}
		if !strings.HasPrefix(t.LiveKitServiceURL, "https://") && !strings.HasPrefix(t.LiveKitServiceURL, "http://") {
			configErrs.Add(fmt.Sprintf("invalid value for config key %q: must be an http or https URL", key+".livekit_service_url"))
		}
	}
}
