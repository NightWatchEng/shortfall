// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package cwinsights

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"
)

// credentials are a static AWS key pair. The full credential-provider chain
// belongs to the SDK; this module stays stdlib-only, and LocalStack (the
// conformance backend) accepts any static pair.
type credentials struct {
	AccessKey string
	SecretKey string
}

// signV4 signs req in place per AWS Signature Version 4 for the Logs
// service: SHA-256 payload hash, canonical headers, and the derived signing
// key. It sets X-Amz-Date and Authorization.
func signV4(req *http.Request, body []byte, creds credentials, region string, now time.Time) {
	signV4svc(req, body, creds, region, "logs", now)
}

// signV4svc is the service-parametrized signer; split out so the known
// AWS test vector (service "service") can pin the algorithm.
func signV4svc(req *http.Request, body []byte, creds credentials, region, service string, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := now.UTC().Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)

	payloadHash := sha256Hex(body)
	headers := map[string]string{
		"host":       req.Host,
		"x-amz-date": amzDate,
	}
	if v := req.Header.Get("Content-Type"); v != "" {
		headers["content-type"] = v
	}

	if v := req.Header.Get("X-Amz-Target"); v != "" {
		headers["x-amz-target"] = v
	}

	if headers["host"] == "" {
		headers["host"] = req.URL.Host
	}

	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}

	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + strings.TrimSpace(headers[k]) + "\n")
	}

	signedHeaders := strings.Join(names, ";")

	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}

	canonical := strings.Join([]string{
		req.Method,
		path,
		req.URL.RawQuery,
		canonHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, region, service, "aws4_request"}, "/")
	toSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonical)),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+creds.SecretKey), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, toSign))

	req.Header.Set("Authorization", strings.Join([]string{
		"AWS4-HMAC-SHA256 Credential=" + creds.AccessKey + "/" + scope,
		"SignedHeaders=" + signedHeaders,
		"Signature=" + signature,
	}, ", "))
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}
