package s3mock

import (
	"encoding/hex"
	"net/url"
	"testing"
)

// The worked examples of the public SigV4 documentation (the fictional
// AKIAIOSFODNN7EXAMPLE key of the AWS documentation, examplebucket).
const (
	docAccessKey = "AKIAIOSFODNN7EXAMPLE"
	docSecret    = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	emptySHA256  = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

func TestSigningKeyDocExample(t *testing.T) {
	got := hex.EncodeToString(signingKey("wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "20120215", "us-east-1", "iam"))
	if want := "f4780e2d9f65fa895f9c67b32ce1baf0b0d8a43505a000a1a9e090d414db404d"; got != want {
		t.Fatalf("signing key = %s, want %s", got, want)
	}
}

func TestHeaderSignatureDocExample(t *testing.T) {
	r := sigRequest{Method: "GET", Path: "/test.txt", Query: url.Values{}, Host: "examplebucket.s3.amazonaws.com",
		Headers:     map[string]string{"range": "bytes=0-9", "x-amz-content-sha256": emptySHA256, "x-amz-date": "20130524T000000Z"},
		PayloadHash: emptySHA256}
	a := parsedAuth{AccessKey: docAccessKey, Date: "20130524", Region: "us-east-1", Service: "s3", AmzDate: "20130524T000000Z",
		SignedHeaders: []string{"host", "range", "x-amz-content-sha256", "x-amz-date"}}
	got := computeSignature(docSecret, r, a)
	if want := "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"; got != want {
		t.Fatalf("GET Object signature = %s, want %s", got, want)
	}
}

func TestPutSignatureDocExample(t *testing.T) {
	// PUT of "Welcome to Amazon S3." to test$file.text, with a Date header and storage class.
	body := "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072"
	r := sigRequest{Method: "PUT", Path: "/test$file.text", Query: url.Values{}, Host: "examplebucket.s3.amazonaws.com",
		Headers: map[string]string{"date": "Fri, 24 May 2013 00:00:00 GMT", "x-amz-date": "20130524T000000Z",
			"x-amz-storage-class": "REDUCED_REDUNDANCY", "x-amz-content-sha256": body},
		PayloadHash: body}
	a := parsedAuth{AccessKey: docAccessKey, Date: "20130524", Region: "us-east-1", Service: "s3", AmzDate: "20130524T000000Z",
		SignedHeaders: []string{"date", "host", "x-amz-content-sha256", "x-amz-date", "x-amz-storage-class"}}
	got := computeSignature(docSecret, r, a)
	if want := "98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd"; got != want {
		t.Fatalf("PUT Object signature = %s, want %s", got, want)
	}
}

func TestListQuerySignatureDocExample(t *testing.T) {
	q := url.Values{"max-keys": {"2"}, "prefix": {"J"}}
	r := sigRequest{Method: "GET", Path: "/", Query: q, Host: "examplebucket.s3.amazonaws.com",
		Headers:     map[string]string{"x-amz-content-sha256": emptySHA256, "x-amz-date": "20130524T000000Z"},
		PayloadHash: emptySHA256}
	a := parsedAuth{AccessKey: docAccessKey, Date: "20130524", Region: "us-east-1", Service: "s3", AmzDate: "20130524T000000Z",
		SignedHeaders: []string{"host", "x-amz-content-sha256", "x-amz-date"}}
	got := computeSignature(docSecret, r, a)
	if want := "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7"; got != want {
		t.Fatalf("GET Bucket (List Objects) signature = %s, want %s", got, want)
	}
}

func TestPresignedSignatureDocExample(t *testing.T) {
	q := url.Values{
		"X-Amz-Algorithm":     {sigAlgorithm},
		"X-Amz-Credential":    {docAccessKey + "/20130524/us-east-1/s3/aws4_request"},
		"X-Amz-Date":          {"20130524T000000Z"},
		"X-Amz-Expires":       {"86400"},
		"X-Amz-SignedHeaders": {"host"},
	}
	r := sigRequest{Method: "GET", Path: "/test.txt", Query: q, Host: "examplebucket.s3.amazonaws.com", PayloadHash: unsignedBody}
	a, ok, err := parseAuth(sigRequest{Query: withSig(q, "x"), Headers: map[string]string{}})
	if !ok || err != nil {
		t.Fatalf("parseAuth: ok=%v err=%v", ok, err)
	}
	got := computeSignature(docSecret, r, a)
	if want := "aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"; got != want {
		t.Fatalf("presigned signature = %s, want %s", got, want)
	}
}

func withSig(q url.Values, sig string) url.Values {
	out := url.Values{}
	for k, v := range q {
		out[k] = v
	}
	out.Set("X-Amz-Signature", sig)
	return out
}
