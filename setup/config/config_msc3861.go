package config

import "strings"

const (
	ClientAuthMethodBasic = "client_secret_basic"
	ClientAuthMethodPost  = "client_secret_post"
)

// MSC3861 configures delegation of authentication to an external OAuth 2.0 / OpenID
// Connect provider, as described in MSC3861. Dendrite then acts purely as a resource
// server: it validates the access tokens issued by the provider through token
// introspection (RFC 7662) and no longer handles logins, registrations or password changes itself.
//
// There are two ways to authenticate to the provider:
//   - with a client ID and secret, for any OpenID Connect provider. Accounts and devices are
//     created the first time a token is seen.
//   - with a shared secret, for Matrix Authentication Service. MAS then manages accounts and
//     devices itself through the /_synapse/mas/ endpoints and tokens for unknown devices are refused.
type MSC3861 struct {
	Enabled bool `yaml:"enabled"`

	// The issuer identifier of the provider, e.g. https://account.example.com/
	Issuer string `yaml:"issuer"`

	// The credentials Dendrite uses to authenticate itself at the introspection endpoint.
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
	// client_secret_basic (default) or client_secret_post.
	ClientAuthMethod string `yaml:"client_auth_method"`

	// Matrix Authentication Service mode. The secret MAS and Dendrite share: Dendrite
	// presents it when introspecting tokens and MAS presents it to the /_synapse/mas/ endpoints.
	SharedSecret string `yaml:"shared_secret"`

	// Optional. Overrides the introspection endpoint advertised in the provider's metadata.
	IntrospectionEndpoint string `yaml:"introspection_endpoint"`
}

// MASMode is true if the provider is Matrix Authentication Service.
func (c *MSC3861) MASMode() bool {
	return c.Enabled && c.SharedSecret != ""
}

func (c *MSC3861) Verify(configErrs *ConfigErrors) {
	if !c.Enabled {
		return
	}
	checkNotEmpty(configErrs, "client_api.msc3861.issuer", c.Issuer)
	if c.SharedSecret == "" {
		checkNotEmpty(configErrs, "client_api.msc3861.client_id", c.ClientID)
		checkNotEmpty(configErrs, "client_api.msc3861.client_secret", c.ClientSecret)
	}
	switch c.ClientAuthMethod {
	case "", ClientAuthMethodBasic, ClientAuthMethodPost:
	default:
		configErrs.Add("config key 'client_api.msc3861.client_auth_method' must be one of '" +
			ClientAuthMethodBasic + "' or '" + ClientAuthMethodPost + "'")
	}
	if c.Issuer != "" && !strings.HasPrefix(c.Issuer, "https://") && !strings.HasPrefix(c.Issuer, "http://") {
		configErrs.Add("config key 'client_api.msc3861.issuer' must be an http(s) URL")
	}
}
