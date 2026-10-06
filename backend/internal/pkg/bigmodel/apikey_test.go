//go:build unit

package bigmodel

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeJSON answers a stub upstream with a canned JSON body.
func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, body)
}

// serveAPIRoutes dispatches to the handler registered for
// "<METHOD> <path>". An unregistered call fails the test: that is how
// "no unexpected request", "never create twice" and "no second direct path"
// are proven rather than assumed.
func serveAPIRoutes(t *testing.T, routes map[string]http.HandlerFunc) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		handler, ok := routes[key]
		if !ok {
			t.Errorf("unexpected upstream call: %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
			return
		}
		handler(w, r)
	}
}

const (
	testAccessToken = "bigmodel-at-1"
	testAPIKey      = "key-abc123"
	testSecretKey   = "secret-xyz"
)

// customerInfoBody answers getCustomerInfo with a non-default organization
// first and a non-default project first, so the selector has to do real work.
const customerInfoBody = `{"code":200,"msg":"ok","data":{"organizations":[` +
	`{"organizationId":"org-2","organizationName":"团队机构","projects":[` +
	`{"projectId":"proj-2","projectName":"团队项目"}]},` +
	`{"organizationId":"org-1","organizationName":"我的默认机构","projects":[` +
	`{"projectId":"proj-9","projectName":"其他项目"},` +
	`{"projectId":"proj-1","projectName":"默认项目"}]}]}}`

// TestResolveZCodeAPIKeyFindsExistingKey pins the resolution chain from
// research FINAL-REPORT §六: getCustomerInfo -> default org -> default project
// -> find "zcode-api-key" -> /copy/{apiKey} -> secretKey, all against
// bigmodel.cn with the raw access token as Authorization.
func TestResolveZCodeAPIKeyFindsExistingKey(t *testing.T) {
	t.Parallel()

	stub := newStubUpstream(serveAPIRoutes(t, map[string]http.HandlerFunc{
		"GET /api/biz/customer/getCustomerInfo": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, customerInfoBody)
		},
		"GET /api/biz/v1/organization/org-1/projects/proj-1/api_keys": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, `{"code":200,"msg":"ok","data":[`+
				`{"name":"team-key","apiKey":"key-team"},`+
				`{"name":"zcode-api-key","apiKey":"`+testAPIKey+`"}]}`)
		},
		"GET /api/biz/v1/organization/org-1/projects/proj-1/api_keys/copy/" + testAPIKey: func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, `{"code":200,"msg":"ok","data":{"secretKey":"`+testSecretKey+`"}}`)
		},
	}))

	// No RiskClient here: bigmodel.cn is not a risk-controlled domain (design M1
	// gates only the zcode.z.ai calls).
	id, secret, err := ResolveZCodeAPIKey(context.Background(), stub, testAccessToken)
	require.NoError(t, err)
	require.Equal(t, testAPIKey, id)
	require.Equal(t, testSecretKey, secret)

	calls := stub.calls()
	require.Equal(t, []string{
		"GET /api/biz/customer/getCustomerInfo",
		"GET /api/biz/v1/organization/org-1/projects/proj-1/api_keys",
		"GET /api/biz/v1/organization/org-1/projects/proj-1/api_keys/copy/" + testAPIKey,
	}, uniquePaths(calls), "the resolution chain must be exactly getCustomerInfo -> api_keys -> copy")
	for _, call := range calls {
		require.Equal(t, testAccessToken, call.Header.Get("Authorization"),
			"Authorization carries the raw access token, never a Bearer prefix")
		require.Equal(t, "application/json", call.Header.Get("Content-Type"))
	}
}

// jsonHandler answers every request with a fixed JSON body and HTTP 200.
func jsonHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, body) }
}

// statusHandler answers every request with a fixed status and body.
func statusHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// apiRouteOverrides replaces individual bigmodel.cn endpoints; nil keeps the
// happy-path default.
type apiRouteOverrides struct {
	customerInfo http.HandlerFunc
	listKeys     http.HandlerFunc
	createKey    http.HandlerFunc
	copyKey      http.HandlerFunc
}

// newAPIStub builds the three-endpoint bigmodel.cn stub. The creation endpoint
// is registered only when a test provides one, so an unexpected creation fails
// the test instead of silently mutating upstream state.
func newAPIStub(t *testing.T, overrides apiRouteOverrides) *stubUpstream {
	t.Helper()

	customerInfo := overrides.customerInfo
	if customerInfo == nil {
		customerInfo = jsonHandler(customerInfoBody)
	}
	listKeys := overrides.listKeys
	if listKeys == nil {
		listKeys = jsonHandler(`{"code":200,"msg":"ok","data":[` +
			`{"name":"team-key","apiKey":"key-team"},` +
			`{"name":"zcode-api-key","apiKey":"` + testAPIKey + `"}]}`)
	}
	copyKey := overrides.copyKey
	if copyKey == nil {
		copyKey = jsonHandler(`{"code":200,"msg":"ok","data":{"secretKey":"` + testSecretKey + `"}}`)
	}

	const apiKeysURL = "GET /api/biz/v1/organization/org-1/projects/proj-1/api_keys"
	routes := map[string]http.HandlerFunc{
		"GET /api/biz/customer/getCustomerInfo": customerInfo,
		apiKeysURL:                              listKeys,
		apiKeysURL + "/copy/" + testAPIKey:      copyKey,
	}
	if overrides.createKey != nil {
		routes["POST /api/biz/v1/organization/org-1/projects/proj-1/api_keys"] = overrides.createKey
	}
	return newStubUpstream(serveAPIRoutes(t, routes))
}

// TestResolveZCodeAPIKeyFailures pins the failure taxonomy of the resolution
// chain: a dead credential (401/403, at the HTTP or body level) stays
// distinguishable from every other failure so the keeper only flags relogin for
// the former.
func TestResolveZCodeAPIKeyFailures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		overrides    apiRouteOverrides
		wantKind     ErrorKind
		wantStatus   int
		wantCode     int
		wantField    string
		authRejected bool
	}{
		{
			name:         "unauthorized",
			overrides:    apiRouteOverrides{customerInfo: statusHandler(http.StatusUnauthorized, `{"code":401,"msg":"invalid token"}`)},
			wantKind:     ErrorKindHTTPStatus,
			wantStatus:   401,
			authRejected: true,
		},
		{
			name:         "forbidden",
			overrides:    apiRouteOverrides{customerInfo: statusHandler(http.StatusForbidden, `{"code":403,"msg":"forbidden"}`)},
			wantKind:     ErrorKindHTTPStatus,
			wantStatus:   403,
			authRejected: true,
		},
		{
			name:         "body level unauthorized",
			overrides:    apiRouteOverrides{customerInfo: jsonHandler(`{"code":401,"msg":"invalid token"}`)},
			wantKind:     ErrorKindUpstreamCode,
			wantStatus:   200,
			wantCode:     401,
			authRejected: true,
		},
		{
			name:       "copy endpoint server error",
			overrides:  apiRouteOverrides{copyKey: statusHandler(http.StatusInternalServerError, `<html>boom</html>`)},
			wantKind:   ErrorKindHTTPStatus,
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:       "non json customer info",
			overrides:  apiRouteOverrides{customerInfo: jsonHandler(`<html>proxy error</html>`)},
			wantKind:   ErrorKindInvalidBody,
			wantStatus: 200,
		},
		{
			name:       "account without organizations",
			overrides:  apiRouteOverrides{customerInfo: jsonHandler(`{"code":200,"msg":"ok","data":{"organizations":[]}}`)},
			wantKind:   ErrorKindMissingField,
			wantStatus: 200,
			wantField:  "data.organizations[].organizationId",
		},
		{
			name: "organization without projects",
			overrides: apiRouteOverrides{customerInfo: jsonHandler(
				`{"code":200,"msg":"ok","data":{"organizations":[{"organizationId":"org-1","organizationName":"默认机构","projects":[]}]}}`)},
			wantKind:   ErrorKindMissingField,
			wantStatus: 200,
			wantField:  "data.organizations[].projects[].projectId",
		},
		{
			name:       "existing key without apiKey",
			overrides:  apiRouteOverrides{listKeys: jsonHandler(`{"code":200,"msg":"ok","data":[{"name":"zcode-api-key"}]}`)},
			wantKind:   ErrorKindMissingField,
			wantStatus: 200,
			wantField:  "data.api_keys[].apiKey",
		},
		{
			name:       "api key list is not an array",
			overrides:  apiRouteOverrides{listKeys: jsonHandler(`{"code":200,"msg":"ok","data":"not-an-array"}`)},
			wantKind:   ErrorKindInvalidBody,
			wantStatus: 200,
			wantField:  "data.api_keys",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			stub := newAPIStub(t, tc.overrides)

			id, secret, err := ResolveZCodeAPIKey(context.Background(), stub, testAccessToken)
			require.Error(t, err)
			require.Empty(t, id)
			require.Empty(t, secret)

			var classified *Error
			require.ErrorAs(t, err, &classified, "error must be classified: %v", err)
			require.Equal(t, "resolve_api_key", classified.Op)
			require.Equal(t, tc.wantKind, classified.Kind)
			require.Equal(t, tc.wantStatus, classified.Status)
			require.Equal(t, tc.wantCode, classified.Code)
			require.Equal(t, tc.wantField, classified.Field)
			require.Equal(t, tc.authRejected, IsAuthRejected(err), "IsAuthRejected mismatch: %v", err)
		})
	}
}

// TestResolveZCodeAPIKeyWithoutSecret covers the tolerated empty private half:
// the copy endpoint may answer without a secretKey, in which case the bare key
// id is the credential and the resolution still succeeds.
func TestResolveZCodeAPIKeyWithoutSecret(t *testing.T) {
	t.Parallel()

	stub := newAPIStub(t, apiRouteOverrides{
		copyKey: jsonHandler(`{"code":200,"msg":"ok","data":{"secretKey":""}}`),
	})

	id, secret, err := ResolveZCodeAPIKey(context.Background(), stub, testAccessToken)
	require.NoError(t, err)
	require.Equal(t, testAPIKey, id)
	require.Empty(t, secret, "an absent secretKey yields the bare key, not an error")
}

// TestResolveZCodeAPIKeyRejectsBadArguments covers the failures decided before
// any upstream call.
func TestResolveZCodeAPIKeyRejectsBadArguments(t *testing.T) {
	t.Parallel()

	t.Run("empty access token", func(t *testing.T) {
		t.Parallel()

		stub := newAPIStub(t, apiRouteOverrides{})
		_, _, err := ResolveZCodeAPIKey(context.Background(), stub, "  ")
		var classified *Error
		require.ErrorAs(t, err, &classified)
		require.Equal(t, ErrorKindInvalidInput, classified.Kind)
		require.Equal(t, "access_token", classified.Field)
		require.Equal(t, 0, stub.callCount())
	})

	t.Run("no doer", func(t *testing.T) {
		t.Parallel()

		_, _, err := ResolveZCodeAPIKey(context.Background(), nil, testAccessToken)
		var classified *Error
		require.ErrorAs(t, err, &classified)
		require.Equal(t, ErrorKindConfig, classified.Kind)
	})

	t.Run("network failure", func(t *testing.T) {
		t.Parallel()

		_, _, err := ResolveZCodeAPIKey(context.Background(),
			failingUpstream{err: errors.New("dial tcp: connection refused")}, testAccessToken)
		var classified *Error
		require.ErrorAs(t, err, &classified)
		require.Equal(t, ErrorKindTransport, classified.Kind)
		require.False(t, IsAuthRejected(err), "a network failure must not look like a dead credential")
	})
}

// TestResolveZCodeAPIKeyEncodesCopyPath pins the copy call's path shape: the
// key id is an opaque value that must travel as exactly one URL path segment
// (the official client feeds it through encodeURIComponent).
func TestResolveZCodeAPIKeyEncodesCopyPath(t *testing.T) {
	t.Parallel()

	const opaqueKey = "key.abc+def/ghi"

	stub := newStubUpstream(serveAPIRoutes(t, map[string]http.HandlerFunc{
		"GET /api/biz/customer/getCustomerInfo": jsonHandler(customerInfoBody),
		"GET /api/biz/v1/organization/org-1/projects/proj-1/api_keys": jsonHandler(
			`{"code":200,"msg":"ok","data":[{"name":"zcode-api-key","apiKey":"` + opaqueKey + `"}]}`),
		"GET /api/biz/v1/organization/org-1/projects/proj-1/api_keys/copy/" + opaqueKey: jsonHandler(
			`{"code":200,"msg":"ok","data":{"secretKey":"secret-opaque"}}`),
	}))

	id, secret, err := ResolveZCodeAPIKey(context.Background(), stub, testAccessToken)
	require.NoError(t, err)
	require.Equal(t, opaqueKey, id)
	require.Equal(t, "secret-opaque", secret)

	calls := stub.calls()
	copyURL := calls[len(calls)-1].URL
	require.Contains(t, copyURL, "/api_keys/copy/key.abc+def%2Fghi",
		"the key id must be percent-encoded into one path segment: %s", copyURL)
}

// TestResolveZCodeAPIKeyCreatesKeyOnce covers the mutating branch: when the
// account has no "zcode-api-key" the resolver creates it with exactly
// {name:"zcode-api-key"} and copies the created id; a second resolution, with
// the key now present, must not create anything again.
func TestResolveZCodeAPIKeyCreatesKeyOnce(t *testing.T) {
	t.Parallel()

	created := false
	var createBodies []string

	stub := newStubUpstream(serveAPIRoutes(t, map[string]http.HandlerFunc{
		"GET /api/biz/customer/getCustomerInfo": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, customerInfoBody)
		},
		"GET /api/biz/v1/organization/org-1/projects/proj-1/api_keys": func(w http.ResponseWriter, _ *http.Request) {
			if !created {
				writeJSON(w, `{"code":200,"msg":"ok","data":[]}`)
				return
			}
			writeJSON(w, `{"code":200,"msg":"ok","data":[{"name":"zcode-api-key","apiKey":"key-new"}]}`)
		},
		"POST /api/biz/v1/organization/org-1/projects/proj-1/api_keys": func(w http.ResponseWriter, r *http.Request) {
			raw, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			createBodies = append(createBodies, string(raw))
			created = true
			writeJSON(w, `{"code":200,"msg":"ok","data":{"name":"zcode-api-key","apiKey":"key-new"}}`)
		},
		"GET /api/biz/v1/organization/org-1/projects/proj-1/api_keys/copy/key-new": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, `{"code":200,"msg":"ok","data":{"secretKey":"secret-new"}}`)
		},
	}))

	id, secret, err := ResolveZCodeAPIKey(context.Background(), stub, testAccessToken)
	require.NoError(t, err)
	require.Equal(t, "key-new", id)
	require.Equal(t, "secret-new", secret)
	require.Len(t, createBodies, 1)
	require.Equal(t, map[string]any{"name": "zcode-api-key"}, jsonBody(t, createBodies[0]),
		"the creation body must be exactly the key name")

	id, secret, err = ResolveZCodeAPIKey(context.Background(), stub, testAccessToken)
	require.NoError(t, err)
	require.Equal(t, "key-new", id)
	require.Equal(t, "secret-new", secret)
	require.Len(t, createBodies, 1, "an existing zcode-api-key must never be created twice")
}
