package bigmodel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	// zcodeAPIKeyName is the key name the official ZCode client looks for and
	// creates when it is absent (research FINAL-REPORT §六).
	zcodeAPIKeyName = "zcode-api-key"

	// defaultOrganizationMarker and defaultProjectMarker select the personal
	// organization/project when the account also belongs to team spaces.
	defaultOrganizationMarker = "默认机构"
	defaultProjectMarker      = "默认项目"

	// customerInfoPath and the organization API prefix live on bigmodel.cn.
	customerInfoPath = "/api/biz/customer/getCustomerInfo"
	organizationPath = "/api/biz/v1/organization"
)

// ResolveZCodeAPIKey resolves the bigmodel API key of a freshly logged-in
// account: getCustomerInfo -> default organization and project -> the
// "zcode-api-key" entry (created only when it does not exist) -> its secret via
// the copy endpoint. It returns the key id and its secret; the secret may be
// empty, in which case the caller uses the bare id (research FINAL-REPORT §六).
//
// Unlike ExchangeCode, these calls live on bigmodel.cn, which is not the
// risk-controlled domain (design M1 gates only zcode.z.ai), so doer is a plain
// HTTP doer. Every failure is a classified *Error; HTTP 401/403 is reported as
// such so the credential keeper can tell a dead credential from a transient
// failure.
func ResolveZCodeAPIKey(ctx context.Context, doer HTTPDoer, accessToken string) (id, secret string, err error) {
	if strings.TrimSpace(accessToken) == "" {
		return "", "", invalidInputError(opResolveAPIKey, "access_token")
	}
	if doer == nil {
		return "", "", configError(opResolveAPIKey, errors.New("no HTTP doer configured"))
	}

	organizationID, projectID, err := resolveDefaultOrganizationProject(ctx, doer, accessToken)
	if err != nil {
		return "", "", err
	}

	apiKeysURL := fmt.Sprintf("%s%s/%s/projects/%s/api_keys",
		bigmodelBaseURL, organizationPath, url.PathEscape(organizationID), url.PathEscape(projectID))

	apiKey, err := resolveAPIKeyEntry(ctx, doer, accessToken, apiKeysURL)
	if err != nil {
		return "", "", err
	}

	secretKey, err := fetchAPIKeySecret(ctx, doer, accessToken, apiKeysURL, apiKey)
	if err != nil {
		return "", "", err
	}
	return apiKey, secretKey, nil
}

// apiOrganization is one entry of data.organizations[].
type apiOrganization struct {
	OrganizationID   string       `json:"organizationId"`
	OrganizationName string       `json:"organizationName"`
	Projects         []apiProject `json:"projects"`
}

// apiProject is one entry of data.organizations[].projects[].
type apiProject struct {
	ProjectID   string `json:"projectId"`
	ProjectName string `json:"projectName"`
}

// customerInfoData is the getCustomerInfo payload.
type customerInfoData struct {
	Organizations []apiOrganization `json:"organizations"`
}

// apiKeyEntry is one entry of data[] from the api_keys endpoint.
type apiKeyEntry struct {
	Name   string `json:"name"`
	APIKey string `json:"apiKey"`
}

// resolveDefaultOrganizationProject picks the organization whose name contains
// "默认机构" (falling back to the first) and inside it the project containing
// "默认项目" (falling back to the first).
func resolveDefaultOrganizationProject(ctx context.Context, doer HTTPDoer, accessToken string) (organizationID, projectID string, err error) {
	response, err := bigmodelCall(ctx, doer, http.MethodGet, bigmodelBaseURL+customerInfoPath, accessToken, nil)
	if err != nil {
		return "", "", err
	}

	var info customerInfoData
	if err := json.Unmarshal(response.Data, &info); err != nil {
		return "", "", &Error{
			Op: opResolveAPIKey, Kind: ErrorKindInvalidBody, Status: response.Status,
			Field: "data.organizations", err: err,
		}
	}

	organization := selectOrganization(info.Organizations)
	if organization == nil || organization.OrganizationID == "" {
		return "", "", &Error{
			Op: opResolveAPIKey, Kind: ErrorKindMissingField, Status: response.Status,
			Field: "data.organizations[].organizationId",
		}
	}
	project := selectProject(organization.Projects)
	if project == nil || project.ProjectID == "" {
		return "", "", &Error{
			Op: opResolveAPIKey, Kind: ErrorKindMissingField, Status: response.Status,
			Field: "data.organizations[].projects[].projectId",
		}
	}
	return organization.OrganizationID, project.ProjectID, nil
}

// selectOrganization prefers the personal ("默认机构") organization.
func selectOrganization(organizations []apiOrganization) *apiOrganization {
	for i := range organizations {
		if strings.Contains(organizations[i].OrganizationName, defaultOrganizationMarker) {
			return &organizations[i]
		}
	}
	if len(organizations) == 0 {
		return nil
	}
	return &organizations[0]
}

// selectProject prefers the personal ("默认项目") project.
func selectProject(projects []apiProject) *apiProject {
	for i := range projects {
		if strings.Contains(projects[i].ProjectName, defaultProjectMarker) {
			return &projects[i]
		}
	}
	if len(projects) == 0 {
		return nil
	}
	return &projects[0]
}

// resolveAPIKeyEntry returns the existing "zcode-api-key" id, creating the key
// only when no entry with that name exists. An existing entry without an apiKey
// is an error rather than a second creation: this endpoint mutates the upstream
// account, so it must never duplicate keys (ticket risk note).
func resolveAPIKeyEntry(ctx context.Context, doer HTTPDoer, accessToken, apiKeysURL string) (string, error) {
	response, err := bigmodelCall(ctx, doer, http.MethodGet, apiKeysURL, accessToken, nil)
	if err != nil {
		return "", err
	}

	var entries []apiKeyEntry
	if err := json.Unmarshal(response.Data, &entries); err != nil {
		return "", &Error{
			Op: opResolveAPIKey, Kind: ErrorKindInvalidBody, Status: response.Status,
			Field: "data.api_keys", err: err,
		}
	}
	for _, entry := range entries {
		if entry.Name != zcodeAPIKeyName {
			continue
		}
		if entry.APIKey == "" {
			return "", &Error{
				Op: opResolveAPIKey, Kind: ErrorKindMissingField, Status: response.Status,
				Field:   "data.api_keys[].apiKey",
				Message: "an existing " + zcodeAPIKeyName + " entry has no apiKey; refusing to create a duplicate",
			}
		}
		return entry.APIKey, nil
	}

	created, err := bigmodelCall(ctx, doer, http.MethodPost, apiKeysURL, accessToken, map[string]string{"name": zcodeAPIKeyName})
	if err != nil {
		return "", err
	}
	var entry apiKeyEntry
	if err := json.Unmarshal(created.Data, &entry); err != nil {
		return "", &Error{
			Op: opResolveAPIKey, Kind: ErrorKindInvalidBody, Status: created.Status,
			Field: "data.apiKey", err: err,
		}
	}
	if entry.APIKey == "" {
		return "", &Error{Op: opResolveAPIKey, Kind: ErrorKindMissingField, Status: created.Status, Field: "data.apiKey"}
	}
	return entry.APIKey, nil
}

// fetchAPIKeySecret reads the private half of a key through the copy endpoint.
// The secret may legitimately be absent, in which case the bare key id is the
// credential and callers get an empty secret (never an error).
func fetchAPIKeySecret(ctx context.Context, doer HTTPDoer, accessToken, apiKeysURL, apiKey string) (string, error) {
	copyURL := apiKeysURL + "/copy/" + url.PathEscape(apiKey)
	response, err := bigmodelCall(ctx, doer, http.MethodGet, copyURL, accessToken, nil)
	if err != nil {
		return "", err
	}
	var payload struct {
		SecretKey string `json:"secretKey"`
	}
	if err := json.Unmarshal(response.Data, &payload); err != nil {
		return "", &Error{
			Op: opResolveAPIKey, Kind: ErrorKindInvalidBody, Status: response.Status,
			Field: "data.secretKey", err: err,
		}
	}
	return payload.SecretKey, nil
}

// apiResponse is one decoded management response: the raw "data" member plus
// the HTTP status it arrived with, so later-stage failures keep reporting it.
type apiResponse struct {
	Data   json.RawMessage
	Status int
}

// apiEnvelope is the {code,msg,data} shape of the bigmodel management API.
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// bigmodelCall performs one authenticated bigmodel.cn management call and
// returns the raw "data" member. The access token is sent as the Authorization
// header value verbatim (not as a Bearer token), matching the official client.
// Non-200 statuses stay distinguishable (so 401/403 can be classified as a dead
// credential) and neither response bodies nor tokens leak into errors.
func bigmodelCall(ctx context.Context, doer HTTPDoer, method, rawURL, accessToken string, requestBody any) (apiResponse, error) {
	var body io.Reader
	if requestBody != nil {
		payload, err := json.Marshal(requestBody)
		if err != nil {
			return apiResponse{}, &Error{Op: opResolveAPIKey, Kind: ErrorKindInvalidInput, err: err}
		}
		body = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return apiResponse{}, &Error{Op: opResolveAPIKey, Kind: ErrorKindInvalidInput, err: err}
	}
	req.Header.Set("Authorization", accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := doer.Do(req)
	if err != nil {
		return apiResponse{}, transportError(opResolveAPIKey, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return apiResponse{}, transportError(opResolveAPIKey, fmt.Errorf("read response: %w", err))
	}
	if resp.StatusCode != http.StatusOK {
		return apiResponse{}, &Error{Op: opResolveAPIKey, Kind: ErrorKindHTTPStatus, Status: resp.StatusCode}
	}

	var envelope apiEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return apiResponse{}, &Error{Op: opResolveAPIKey, Kind: ErrorKindInvalidBody, Status: resp.StatusCode, err: err}
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		if envelope.Code != 0 {
			return apiResponse{}, &Error{
				Op: opResolveAPIKey, Kind: ErrorKindUpstreamCode, Status: resp.StatusCode,
				Code: envelope.Code, Message: truncateMessage(envelope.Msg),
			}
		}
		return apiResponse{}, &Error{Op: opResolveAPIKey, Kind: ErrorKindMissingField, Status: resp.StatusCode, Field: "data"}
	}
	return apiResponse{Data: envelope.Data, Status: resp.StatusCode}, nil
}
