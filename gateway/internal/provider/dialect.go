package provider

import (
	"fmt"
	"net/http"

	"kaiak/internal/config"
)

// dialect is what one backend type does differently while speaking the OpenAI wire
// format: where its API lives, how it takes the gateway's credential, what its models
// list names, and what an absent service tier means to it. The request, response,
// streaming and accounting code is the same for every type and never asks which type
// it serves; a difference between types becomes a field here.
type dialect struct {
	// apiPrefix is appended to the backend's base_url before an endpoint path
	// (docs/specs/GATEWAY.md, Base URLs): an openai-compatible base URL already ends
	// in the API version path; an azure-openai one is the resource endpoint.
	apiPrefix string
	// credentialHeader carries the gateway's credential, as credentialPrefix +
	// credential.
	credentialHeader string
	credentialPrefix string
	// listsDeployments reports whether the models list names what requests carry. An
	// azure-openai list names models, not deployment names, so the probe cannot tell
	// whether a deployment exists there.
	listsDeployments bool
	// absentTierFollowsDeployment reports whether a chat completions request without
	// service_tier runs on the tier the deployment is configured with (Azure's auto,
	// possibly priority), so the gateway must name the standard tier itself
	// (docs/specs/GATEWAY.md, Providers → Service tier).
	absentTierFollowsDeployment bool
}

var dialects = map[config.BackendType]dialect{
	config.BackendOpenAICompatible: {
		credentialHeader: "Authorization",
		credentialPrefix: "Bearer ",
		listsDeployments: true,
	},
	config.BackendAzureOpenAI: {
		apiPrefix:                   "/openai/v1",
		credentialHeader:            "Api-Key",
		absentTierFollowsDeployment: true,
	},
}

// dialectOf returns backend b's dialect. The config schema admits only the types
// listed in dialects, so a missing one is a gateway fault.
func dialectOf(b *config.Backend) dialect {
	d, ok := dialects[b.Type]
	if !ok {
		panic(fmt.Sprintf("provider: no dialect for backend type %q", b.Type))
	}
	return d
}

// url joins baseURL and path, a path below the OpenAI API's version prefix.
func (d dialect) url(baseURL, path string) string {
	return baseURL + d.apiPrefix + "/" + path
}

// setCredential sets the gateway's credential, if it has one.
func (d dialect) setCredential(h http.Header, credential string) {
	if credential == "" {
		return
	}
	h.Set(d.credentialHeader, d.credentialPrefix+credential)
}
