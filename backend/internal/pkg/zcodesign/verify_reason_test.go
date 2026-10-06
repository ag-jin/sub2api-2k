package zcodesign

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Protocol literals copied from the official CLI bundle
// (workspace: zcode-research/official-app/beautified/zcode.cjs:32438-32443,
// function readRefreshableClientSignatureReason). Kept as literals so the
// assertions below are independent from the package constants under test.
const (
	protocolVerifySignatureInvalid = "VERIFY_SIGNATURE_INVALID"
	protocolVerifyAPIKeyExpired    = "VERIFY_APIKEY_EXPIRED"
)

func TestVerifyReasonConstantsMatchProtocol(t *testing.T) {
	t.Parallel()

	assert.Equal(t, protocolVerifySignatureInvalid, VerifySignatureInvalid)
	assert.Equal(t, protocolVerifyAPIKeyExpired, VerifyAPIKeyExpired)
}

func TestReadZhipuSignatureVerifyReason(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "msg signature invalid",
			body: `{"msg":"VERIFY_SIGNATURE_INVALID"}`,
			want: protocolVerifySignatureInvalid,
		},
		{
			name: "msg apikey expired",
			body: `{"msg":"VERIFY_APIKEY_EXPIRED"}`,
			want: protocolVerifyAPIKeyExpired,
		},
		{
			name: "reason signature invalid",
			body: `{"reason":"VERIFY_SIGNATURE_INVALID"}`,
			want: protocolVerifySignatureInvalid,
		},
		{
			name: "reason apikey expired",
			body: `{"reason":"VERIFY_APIKEY_EXPIRED"}`,
			want: protocolVerifyAPIKeyExpired,
		},
		{
			name: "data reason signature invalid",
			body: `{"data":{"reason":"VERIFY_SIGNATURE_INVALID"}}`,
			want: protocolVerifySignatureInvalid,
		},
		{
			name: "data reason apikey expired",
			body: `{"data":{"reason":"VERIFY_APIKEY_EXPIRED"}}`,
			want: protocolVerifyAPIKeyExpired,
		},
		{
			name: "error reason signature invalid",
			body: `{"error":{"reason":"VERIFY_SIGNATURE_INVALID"}}`,
			want: protocolVerifySignatureInvalid,
		},
		{
			name: "error reason apikey expired",
			body: `{"error":{"reason":"VERIFY_APIKEY_EXPIRED"}}`,
			want: protocolVerifyAPIKeyExpired,
		},
		{
			name: "error message signature invalid",
			body: `{"error":{"message":"VERIFY_SIGNATURE_INVALID"}}`,
			want: protocolVerifySignatureInvalid,
		},
		{
			name: "error message apikey expired",
			body: `{"error":{"message":"VERIFY_APIKEY_EXPIRED"}}`,
			want: protocolVerifyAPIKeyExpired,
		},
		{
			name: "empty body",
			body: ``,
			want: "",
		},
		{
			name: "plain text error page",
			body: `<html><body>502 Bad Gateway</body></html>`,
			want: "",
		},
		{
			name: "valid json without verify fields",
			body: `{"code":500,"msg":"internal error","error":{"message":"upstream failed"}}`,
			want: "",
		},
		{
			name: "lowercase reason variant",
			body: `{"msg":"verify_signature_invalid","reason":"verify_apikey_expired"}`,
			want: "",
		},
		{
			name: "mixed case reason variant",
			body: `{"error":{"message":"Verify_Signature_Invalid"}}`,
			want: "",
		},
		{
			name: "reason as suffix of longer string",
			body: `{"reason":"client reported VERIFY_SIGNATURE_INVALID today"}`,
			want: "",
		},
		{
			name: "reason with trailing characters",
			body: `{"reason":"VERIFY_SIGNATURE_INVALID_EXTRA"}`,
			want: "",
		},
		{
			name: "data error message is not a match path",
			body: `{"data":{"error":{"message":"VERIFY_SIGNATURE_INVALID"},"reason":"ok"}}`,
			want: "",
		},
		{
			name: "error as string is not a match path",
			body: `{"error":"VERIFY_SIGNATURE_INVALID"}`,
			want: "",
		},
		{
			name: "top level json string",
			body: `"VERIFY_SIGNATURE_INVALID"`,
			want: "",
		},
		{
			name: "top level json array",
			body: `["VERIFY_SIGNATURE_INVALID"]`,
			want: "",
		},
		{
			name: "data as array is not a match path",
			body: `{"data":[{"reason":"VERIFY_APIKEY_EXPIRED"}]}`,
			want: "",
		},
		{
			name: "msg as number",
			body: `{"msg":401}`,
			want: "",
		},
		{
			name: "msg as bool",
			body: `{"msg":true}`,
			want: "",
		},
		{
			name: "msg as array",
			body: `{"msg":["VERIFY_SIGNATURE_INVALID"]}`,
			want: "",
		},
		{
			name: "reason as object",
			body: `{"reason":{"text":"VERIFY_SIGNATURE_INVALID"}}`,
			want: "",
		},
		{
			name: "reason as null",
			body: `{"reason":null}`,
			want: "",
		},
		{
			name: "error as null with null msg",
			body: `{"error":null,"msg":null}`,
			want: "",
		},
		{
			name: "error reason as array",
			body: `{"error":{"reason":["VERIFY_APIKEY_EXPIRED"]}}`,
			want: "",
		},
		{
			name: "error message as object",
			body: `{"error":{"message":{"reason":"VERIFY_SIGNATURE_INVALID"}}}`,
			want: "",
		},
		{
			name: "unknown nested structure",
			body: `{"result":{"payload":{"detail":{"code":"VERIFY_SIGNATURE_INVALID"}}}}`,
			want: "",
		},
		{
			name: "noisy body with reason hit",
			body: `{"noise":"` + strings.Repeat("x", 1<<20) + `","reason":"VERIFY_SIGNATURE_INVALID"}`,
			want: protocolVerifySignatureInvalid,
		},
		{
			name: "noisy body without hit",
			body: `{"noise":"` + strings.Repeat("y", 1<<20) + `","reason":"upstream overloaded"}`,
			want: "",
		},
		{
			name: "msg apikey expired wins over error message signature invalid",
			body: `{"msg":"VERIFY_APIKEY_EXPIRED","error":{"message":"VERIFY_SIGNATURE_INVALID"}}`,
			want: protocolVerifyAPIKeyExpired,
		},
		{
			name: "reason signature invalid wins over error message apikey expired",
			body: `{"reason":"VERIFY_SIGNATURE_INVALID","error":{"message":"VERIFY_APIKEY_EXPIRED"}}`,
			want: protocolVerifySignatureInvalid,
		},
		{
			name: "data reason signature invalid wins over error reason apikey expired",
			body: `{"data":{"reason":"VERIFY_SIGNATURE_INVALID"},"error":{"reason":"VERIFY_APIKEY_EXPIRED"}}`,
			want: protocolVerifySignatureInvalid,
		},
		{
			name: "error reason apikey expired wins over error message signature invalid",
			body: `{"error":{"reason":"VERIFY_APIKEY_EXPIRED","message":"VERIFY_SIGNATURE_INVALID"}}`,
			want: protocolVerifyAPIKeyExpired,
		},
		{
			name: "json key order does not change field precedence",
			body: `{"reason":"VERIFY_SIGNATURE_INVALID","msg":"VERIFY_APIKEY_EXPIRED"}`,
			want: protocolVerifyAPIKeyExpired,
		},
	}

	entries := []struct {
		entry string
		fn    func([]byte) string
	}{
		{"readZhipuSignatureVerifyReason", readZhipuSignatureVerifyReason},
		{"ReadZhipuSignatureVerifyReason", ReadZhipuSignatureVerifyReason},
	}

	for _, e := range entries {
		t.Run(e.entry, func(t *testing.T) {
			t.Parallel()
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					assert.Equal(t, tc.want, e.fn([]byte(tc.body)))
				})
			}
		})
	}
}

// BenchmarkReadZhipuSignatureVerifyReasonLongBody pins the single-scan cost:
// the parser decodes the body once and never revisits it.
func BenchmarkReadZhipuSignatureVerifyReasonLongBody(b *testing.B) {
	body := []byte(`{"noise":"` + strings.Repeat("x", 1<<20) + `","error":{"message":"VERIFY_SIGNATURE_INVALID"}}`)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		ReadZhipuSignatureVerifyReason(body)
	}
}
