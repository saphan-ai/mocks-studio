package ocimock

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

// The fictional credentials the token endpoint accepts. They are public on purpose: the
// mock guards nothing, it lets a client rehearse a login.
const (
	Username = "acme-user"
	Password = "acme-example-password"
)

const (
	serviceName = "oci-mock"
	tokenTTL    = 3600
)

// registryError is one entry of the specification's error document.
type registryError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  any    `json:"detail,omitempty"`
}

// regErr writes the specification's JSON error form.
func regErr(w http.ResponseWriter, status int, code, message string, detail any) {
	writeJSON(w, status, map[string]any{"errors": []registryError{{Code: code, Message: message, Detail: detail}}})
}

// bearerOK reports whether the request carries a token this scope issued.
func (s *scope) bearerOK(r *http.Request) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, known := s.tokens[tok]
	return known
}

// challenge answers 401 with the bearer challenge a client follows to the token endpoint.
func (s *scope) challenge(rec *recorder, r *http.Request, rt route) {
	c := `Bearer realm="http://` + r.Host + `/token",service="` + serviceName + `"`
	if rt.name != "" && validName(rt.name) {
		action := "pull"
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			action = "pull,push"
		}
		c += `,scope="repository:` + rt.name + `:` + action + `"`
	}
	if r.Header.Get("Authorization") != "" {
		c += `,error="invalid_token"`
	}
	rec.Header().Set("WWW-Authenticate", c)
	rec.fault = settingAuthRequired
	regErr(rec, http.StatusUnauthorized, "UNAUTHORIZED", "The request carries no valid bearer token and was refused; fetch a token from the realm in WWW-Authenticate with the registry credentials and retry.", nil)
}

// handleToken issues a bearer token for the fictional credentials.
func (s *scope) handleToken(w http.ResponseWriter, r *http.Request) {
	user, pass, ok := r.BasicAuth()
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(Username)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(Password)) == 1
	if !ok || !userOK || !passOK {
		w.Header().Set("WWW-Authenticate", `Basic realm="`+serviceName+`"`)
		regErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "The token request carries no valid registry credentials and was refused; send the documented username and password as HTTP Basic.", nil)
		return
	}
	tok := randomHex(24)
	s.mu.Lock()
	s.tokens[tok] = struct{}{}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"token": tok, "access_token": tok, "expires_in": tokenTTL})
}
