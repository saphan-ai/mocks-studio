package s3mock

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AWS Signature Version 4, written from the public documentation of the signing
// process, with the standard library only.

const (
	sigAlgorithm   = "AWS4-HMAC-SHA256"
	sigTerminator  = "aws4_request"
	sigService     = "s3"
	amzDateLayout  = "20060102T150405Z"
	amzShortLayout = "20060102"
	unsignedBody   = "UNSIGNED-PAYLOAD"
	maxSkew        = 15 * time.Minute
	maxPresignTTL  = 7 * 24 * time.Hour
)

// sigRequest is the part of an HTTP request that SigV4 covers.
type sigRequest struct {
	Method      string
	Path        string // decoded path, as the server saw it
	Query       url.Values
	Headers     map[string]string // lower-case name -> raw value
	Host        string
	PayloadHash string // hex digest, or UNSIGNED-PAYLOAD
}

// parsedAuth is what a request says about how it was signed.
type parsedAuth struct {
	AccessKey     string
	Date          string // yyyymmdd of the credential scope
	Region        string
	Service       string
	SignedHeaders []string
	Signature     string
	AmzDate       string
	Presigned     bool
	Expires       time.Duration
}

// Refusal reasons of the signature check; each maps to one S3 error code.
var (
	errAuthMalformed     = errors.New("AuthorizationHeaderMalformed")
	errQueryMalformed    = errors.New("AuthorizationQueryParametersError")
	errAccessKeyUnknown  = errors.New("InvalidAccessKeyId")
	errSignatureMismatch = errors.New("SignatureDoesNotMatch")
	errExpired           = errors.New("ExpiredRequest")
	errSkewed            = errors.New("RequestTimeTooSkewed")
)

// awsEncode is the URI encoding SigV4 specifies: every byte except the unreserved
// set is %XX in upper case; '/' is kept only when keepSlash.
func awsEncode(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func canonicalQuery(q url.Values, skipSignature bool) string {
	type pair struct{ k, v string }
	var pairs []pair
	for k, vs := range q {
		if skipSignature && k == "X-Amz-Signature" {
			continue
		}
		for _, v := range vs {
			pairs = append(pairs, pair{awsEncode(k, false), awsEncode(v, false)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = p.k + "=" + p.v
	}
	return strings.Join(parts, "&")
}

func canonicalHeaderValue(v string) string { return strings.Join(strings.Fields(v), " ") }

func canonicalRequest(r sigRequest, a parsedAuth) string {
	var hdr strings.Builder
	for _, name := range a.SignedHeaders {
		v := r.Headers[name]
		if name == "host" {
			v = r.Host
		}
		hdr.WriteString(name + ":" + canonicalHeaderValue(v) + "\n")
	}
	return strings.Join([]string{
		r.Method,
		awsEncode(r.Path, true),
		canonicalQuery(r.Query, a.Presigned),
		hdr.String(),
		strings.Join(a.SignedHeaders, ";"),
		r.PayloadHash,
	}, "\n")
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg)) // hash.Hash.Write never fails
	return m.Sum(nil)
}

func signingKey(secret, date, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, sigTerminator)
}

func stringToSign(canonical string, a parsedAuth) string {
	sum := sha256.Sum256([]byte(canonical))
	scope := strings.Join([]string{a.Date, a.Region, a.Service, sigTerminator}, "/")
	return strings.Join([]string{sigAlgorithm, a.AmzDate, scope, hex.EncodeToString(sum[:])}, "\n")
}

// computeSignature is the hex signature a request must carry for the secret.
func computeSignature(secret string, r sigRequest, a parsedAuth) string {
	sts := stringToSign(canonicalRequest(r, a), a)
	return hex.EncodeToString(hmacSHA256(signingKey(secret, a.Date, a.Region, a.Service), sts))
}

// parseCredential splits "<key>/<date>/<region>/<service>/aws4_request".
func parseCredential(c string, a *parsedAuth) bool {
	parts := strings.Split(c, "/")
	if len(parts) != 5 || parts[4] != sigTerminator || parts[0] == "" {
		return false
	}
	a.AccessKey, a.Date, a.Region, a.Service = parts[0], parts[1], parts[2], parts[3]
	return true
}

// parseAuth reads the signing information from the Authorization header or, when
// there is none, from the presign query parameters. ok is false when the request
// carries no signature of either kind.
func parseAuth(r sigRequest) (a parsedAuth, ok bool, err error) {
	if h := r.Headers["authorization"]; h != "" {
		a.AmzDate = r.Headers["x-amz-date"]
		rest, found := strings.CutPrefix(h, sigAlgorithm+" ")
		if !found {
			return a, true, errAuthMalformed
		}
		fields := map[string]string{}
		for _, part := range strings.Split(rest, ",") {
			k, v, cut := strings.Cut(strings.TrimSpace(part), "=")
			if cut {
				fields[k] = v
			}
		}
		if !parseCredential(fields["Credential"], &a) || fields["SignedHeaders"] == "" || fields["Signature"] == "" {
			return a, true, errAuthMalformed
		}
		a.SignedHeaders = strings.Split(fields["SignedHeaders"], ";")
		a.Signature = fields["Signature"]
		if a.AmzDate == "" {
			a.AmzDate = r.Headers["date"]
		}
		return a, true, nil
	}
	if r.Query.Get("X-Amz-Signature") == "" && r.Query.Get("X-Amz-Algorithm") == "" {
		return a, false, nil
	}
	a.Presigned = true
	a.AmzDate = r.Query.Get("X-Amz-Date")
	secs, convErr := strconv.Atoi(r.Query.Get("X-Amz-Expires"))
	if r.Query.Get("X-Amz-Algorithm") != sigAlgorithm || !parseCredential(r.Query.Get("X-Amz-Credential"), &a) ||
		r.Query.Get("X-Amz-SignedHeaders") == "" || r.Query.Get("X-Amz-Signature") == "" || convErr != nil || secs < 1 || time.Duration(secs)*time.Second > maxPresignTTL {
		return a, true, errQueryMalformed
	}
	a.Expires = time.Duration(secs) * time.Second
	a.SignedHeaders = strings.Split(r.Query.Get("X-Amz-SignedHeaders"), ";")
	a.Signature = r.Query.Get("X-Amz-Signature")
	return a, true, nil
}

// verify checks a parsed signature against the scope's credentials and the clock.
func verify(r sigRequest, a parsedAuth, accessKey, secret string, now time.Time) error {
	malformed := errAuthMalformed
	if a.Presigned {
		malformed = errQueryMalformed
	}
	if a.Service != sigService {
		return malformed
	}
	t, perr := time.Parse(amzDateLayout, a.AmzDate)
	if perr != nil || a.Date != t.Format(amzShortLayout) {
		return malformed
	}
	if a.AccessKey != accessKey {
		return errAccessKeyUnknown
	}
	got, derr := hex.DecodeString(a.Signature)
	want, _ := hex.DecodeString(computeSignature(secret, r, a))
	if derr != nil || !hmac.Equal(got, want) {
		return errSignatureMismatch
	}
	if a.Presigned {
		if now.After(t.Add(a.Expires)) {
			return errExpired
		}
		if t.After(now.Add(maxSkew)) {
			return errSkewed
		}
	} else if d := now.Sub(t); d > maxSkew || d < -maxSkew {
		return errSkewed
	}
	return nil
}

// SignHeaders returns the Authorization header value for a request, for callers
// (tests, agents) that sign requests to this mock. headers are lower-case name -> value
// and must include every signed header except host.
func SignHeaders(accessKey, secret, region string, at time.Time, r sigRequest, signed []string) string {
	a := parsedAuth{AccessKey: accessKey, Date: at.UTC().Format(amzShortLayout), Region: region, Service: sigService,
		SignedHeaders: signed, AmzDate: at.UTC().Format(amzDateLayout)}
	sig := computeSignature(secret, r, a)
	return fmt.Sprintf("%s Credential=%s/%s/%s/%s/%s, SignedHeaders=%s, Signature=%s",
		sigAlgorithm, accessKey, a.Date, region, sigService, sigTerminator, strings.Join(signed, ";"), sig)
}

// PresignQuery returns the query string of a presigned URL for method and path
// (host is the Host the client will use).
func PresignQuery(accessKey, secret, region string, at time.Time, ttl time.Duration, method, path, host string) string {
	a := parsedAuth{AccessKey: accessKey, Date: at.UTC().Format(amzShortLayout), Region: region, Service: sigService,
		SignedHeaders: []string{"host"}, AmzDate: at.UTC().Format(amzDateLayout), Presigned: true, Expires: ttl}
	q := url.Values{}
	q.Set("X-Amz-Algorithm", sigAlgorithm)
	q.Set("X-Amz-Credential", fmt.Sprintf("%s/%s/%s/%s/%s", accessKey, a.Date, region, sigService, sigTerminator))
	q.Set("X-Amz-Date", a.AmzDate)
	q.Set("X-Amz-Expires", strconv.Itoa(int(ttl/time.Second)))
	q.Set("X-Amz-SignedHeaders", "host")
	sig := computeSignature(secret, sigRequest{Method: method, Path: path, Query: q, Host: host, PayloadHash: unsignedBody}, a)
	return canonicalQuery(q, false) + "&X-Amz-Signature=" + sig
}
