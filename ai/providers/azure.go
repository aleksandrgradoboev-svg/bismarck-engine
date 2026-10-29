package providers

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
)

// The azure provider's endpoint, ported from pi's api/azure-openai-config.ts
// and providers/azure.ts (upstream a37306d4). Azure models ship without a
// baseUrl: one resource per user, resolved per request. pi's azure provider
// wraps openai-completions in a stream that resolves the endpoint onto the
// model and swaps the deployment name into the request before dispatch; the
// port dispatches by api, so the completions adapter does both for
// model.Provider "azure" (StreamOpenAICompletions). The Responses half
// (azure-openai-responses) is Scope queue row 4 and reads the same options.

// AzureEndpointOptions are pi's AzureEndpointOptions: where an azure request
// goes and under which deployment name. Empty fields fall back to the
// environment (AZURE_OPENAI_BASE_URL, AZURE_OPENAI_RESOURCE_NAME,
// AZURE_OPENAI_DEPLOYMENT_NAME_MAP), then to the model.
type AzureEndpointOptions struct {
	// APIVersion is pi's azureApiVersion (AZURE_OPENAI_API_VERSION, default
	// "v1"). Only the Responses api sends it; Chat Completions does not.
	APIVersion string
	// ResourceName builds https://<ResourceName>.openai.azure.com/openai/v1
	// when no base URL is given.
	ResourceName string
	BaseURL      string
	// DeploymentName is sent as the request's model; the message keeps the
	// catalog id.
	DeploymentName string
}

// parseAzureDeploymentNameMap reads AZURE_OPENAI_DEPLOYMENT_NAME_MAP:
// comma-separated modelId=deploymentName entries. As JavaScript's
// split("=", 2) does, anything after a second "=" is dropped.
func parseAzureDeploymentNameMap(value string) map[string]string {
	out := map[string]string{}
	for _, entry := range strings.Split(value, ",") {
		trimmed := jstext.Trim(entry)
		if trimmed == "" {
			continue
		}
		parts := strings.Split(trimmed, "=")
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			continue
		}
		out[jstext.Trim(parts[0])] = jstext.Trim(parts[1])
	}
	return out
}

// resolveAzureDeploymentName is pi's resolveDeploymentName.
func resolveAzureDeploymentName(model *ai.Model, opts AzureEndpointOptions, env map[string]string) string {
	if opts.DeploymentName != "" {
		return opts.DeploymentName
	}
	if mapped := parseAzureDeploymentNameMap(getProviderEnvValue("AZURE_OPENAI_DEPLOYMENT_NAME_MAP", env))[model.ID]; mapped != "" {
		return mapped
	}
	return model.ID
}

// normalizeAzureBaseURL is pi's normalizeAzureBaseUrl: an Azure host whose
// path is empty, "/openai" or "/openai/v1/responses" gets /openai/v1 as its
// base path, with the query dropped; every trailing slash goes.
func normalizeAzureBaseURL(baseURL string) (string, error) {
	trimmed := strings.TrimRight(jstext.Trim(baseURL), "/")
	parsed, err := requestURL(trimmed)
	if err != nil {
		return "", fmt.Errorf("Invalid Azure OpenAI base URL: %s", baseURL)
	}
	u, err := url.Parse(parsed)
	if err != nil {
		return "", fmt.Errorf("Invalid Azure OpenAI base URL: %s", baseURL)
	}
	host := u.Hostname()
	isAzureHost := strings.HasSuffix(host, ".openai.azure.com") ||
		strings.HasSuffix(host, ".cognitiveservices.azure.com") ||
		strings.HasSuffix(host, ".ai.azure.com")
	path := strings.TrimRight(u.EscapedPath(), "/")
	if isAzureHost && (path == "" || path == "/openai" || path == "/openai/v1/responses") {
		u.Path, u.RawPath = "/openai/v1", ""
		u.RawQuery, u.ForceQuery = "", false
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// resolveAzureBaseURL is pi's resolveAzureBaseUrl: the options' base URL,
// then AZURE_OPENAI_BASE_URL, then one built from the resource name (the
// options', then AZURE_OPENAI_RESOURCE_NAME), then the model's.
func resolveAzureBaseURL(model *ai.Model, opts AzureEndpointOptions, env map[string]string) (string, error) {
	baseURL := jstext.Trim(opts.BaseURL)
	if baseURL == "" {
		baseURL = jstext.Trim(getProviderEnvValue("AZURE_OPENAI_BASE_URL", env))
	}
	resourceName := opts.ResourceName
	if resourceName == "" {
		resourceName = getProviderEnvValue("AZURE_OPENAI_RESOURCE_NAME", env)
	}
	if baseURL == "" && resourceName != "" {
		baseURL = "https://" + resourceName + ".openai.azure.com/openai/v1"
	}
	if baseURL == "" {
		baseURL = model.BaseURL
	}
	if baseURL == "" {
		return "", errors.New("Azure OpenAI base URL is required. Set AZURE_OPENAI_BASE_URL or AZURE_OPENAI_RESOURCE_NAME, or pass azureBaseUrl, azureResourceName, or model.baseUrl.")
	}
	return normalizeAzureBaseURL(baseURL)
}
