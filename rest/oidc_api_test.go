package rest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/couchbase/sync_gateway/auth"
	"github.com/couchbase/sync_gateway/base"
	"github.com/couchbase/sync_gateway/db"
	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/square/go-jose.v2"
	"gopkg.in/square/go-jose.v2/jwt"
)

var (
	tokenTypeBearer       = "Bearer"
	mockSyncGatewayURL    = ""
	issuerGoogle          = ""
	issuerFacebook        = ""
	authProvider          = "provider"
	providerNameGoogle    = "google"
	providerNameFacebook  = "facebook"
	clientIDGoogle        = "client.id.google"
	clientIDFacebook      = "client.id.facebook"
	validationKeyFacebook = "validation.key.facebook"
	validationKeyGoogle   = "validation.key.google"
	wellKnownPath         = "/.well-known/openid-configuration"

	wantTokenResponse OIDCTokenResponse
)

func mockAuthServer() (*httptest.Server, error) {
	router := mux.NewRouter()
	router.HandleFunc("/google"+wellKnownPath, mockDiscoveryHandlerGoogle).Methods(http.MethodGet)
	router.HandleFunc("/google/auth", mockAuthHandler).Methods(http.MethodGet, http.MethodPost)
	router.HandleFunc("/google/token", mockTokenHandler).Methods(http.MethodPost)
	router.HandleFunc("/facebook"+wellKnownPath, mockDiscoveryHandlerFacebook).Methods(http.MethodGet)
	return httptest.NewServer(router), nil
}

func mockDiscoveryHandlerGoogle(res http.ResponseWriter, req *http.Request) {
	metadata := auth.ProviderMetadata{
		Issuer: issuerGoogle, AuthorizationEndpoint: issuerGoogle + "/auth",
		TokenEndpoint: issuerGoogle + "/token", JwksUri: issuerGoogle + "/oauth2/v3/certs",
		IdTokenSigningAlgValuesSupported: []string{"RS256"},
	}
	renderJSON(res, req, http.StatusOK, metadata)
}

func mockDiscoveryHandlerFacebook(res http.ResponseWriter, req *http.Request) {
	metadata := auth.ProviderMetadata{
		Issuer: issuerFacebook, AuthorizationEndpoint: issuerFacebook + "/auth",
		TokenEndpoint: issuerFacebook + "/token", JwksUri: issuerFacebook + "/oauth2/v3/certs",
		IdTokenSigningAlgValuesSupported: []string{"RS256"},
	}
	renderJSON(res, req, http.StatusOK, metadata)
}

func renderJSON(res http.ResponseWriter, req *http.Request, statusCode int, data interface{}) {
	res.Header().Set("Content-Type", "application/json")
	res.WriteHeader(statusCode)
	if err := json.NewEncoder(res).Encode(data); err != nil {
		base.Errorf("Error rendering JSON response: %s", err)
		res.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func mockAuthHandler(res http.ResponseWriter, req *http.Request) {
	var redirectionURL string
	state := req.URL.Query().Get(requestParamState)
	redirect := req.URL.Query().Get(requestParamRedirectURI)
	if redirect == "" {
		base.Errorf("No redirect URL found in auth request")
		res.WriteHeader(http.StatusInternalServerError)
		return
	}
	redirectionURL = fmt.Sprintf("%s?code=%s", redirect, base.GenerateRandomSecret())
	if state != "" {
		redirectionURL = fmt.Sprintf("%s&state=%s", redirectionURL, state)
	}
	http.Redirect(res, req, redirectionURL, http.StatusTemporaryRedirect)
}

func mockTokenHandler(res http.ResponseWriter, req *http.Request) {
	claims := jwt.Claims{ID: "id0123456789", Issuer: issuerGoogle,
		Audience: jwt.Audience{"aud1", "aud2", "aud3", clientIDGoogle},
		IssuedAt: jwt.NewNumericDate(time.Now()), Subject: "noah",
		Expiry: jwt.NewNumericDate(time.Now().Add(5 * time.Minute)),
	}
	signer, err := getRSASigner()
	if err != nil {
		base.Errorf("Error creating RSA signer: %s", err)
		res.WriteHeader(http.StatusInternalServerError)
		return
	}
	claimEmail := map[string]interface{}{"email": "noah@foo.com"}
	builder := jwt.Signed(signer).Claims(claims).Claims(claimEmail)
	token, err := builder.CompactSerialize()
	if err != nil {
		base.Errorf("Error serializing token: %s", err)
		res.WriteHeader(http.StatusInternalServerError)
		return
	}
	response := OIDCTokenResponse{
		IDToken:      token,
		AccessToken:  token,
		RefreshToken: token,
		TokenType:    tokenTypeBearer,
		Expires:      time.Now().Add(5 * time.Minute).UTC().Second(),
	}
	wantTokenResponse = response
	renderJSON(res, req, http.StatusOK, response)
}

func getRSASigner() (signer jose.Signer, err error) {
	rsaPrivateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return signer, err
	}
	signingKey := jose.SigningKey{Algorithm: jose.RS256, Key: rsaPrivateKey}
	var signerOptions = jose.SignerOptions{}
	signerOptions.WithType("JWT")
	signer, err = jose.NewSigner(signingKey, &signerOptions)
	if err != nil {
		return signer, err
	}
	return signer, nil
}

func TestGetOIDCCallbackURL(t *testing.T) {
	authServer, err := mockAuthServer()
	require.NoError(t, err, "Error mocking fake authorization server")
	defer authServer.Close()

	issuerGoogle = authServer.URL + "/google"
	issuerFacebook = authServer.URL + "/facebook"

	// Default OpenID Connect Provider
	providerGoogle := auth.OIDCProvider{
		Name: providerNameGoogle, Issuer: issuerGoogle, ClientID: clientIDGoogle,
		ValidationKey: &validationKeyGoogle, DiscoveryURI: issuerGoogle + wellKnownPath,
	}

	// Non-default OpenID Connect Provider
	providerFacebook := auth.OIDCProvider{
		Name: providerNameFacebook, Issuer: issuerFacebook, ClientID: clientIDFacebook,
		ValidationKey: &validationKeyFacebook, DiscoveryURI: issuerFacebook + wellKnownPath,
	}

	providers := auth.OIDCProviderMap{providerNameGoogle: &providerGoogle, providerNameFacebook: &providerFacebook}
	openIDConnectOptions := auth.OIDCOptions{Providers: providers, DefaultProvider: &providerGoogle.Name}
	rtConfig := RestTesterConfig{DatabaseConfig: &DbConfig{OIDCConfig: &openIDConnectOptions}}
	rt := NewRestTester(t, &rtConfig)
	defer rt.Close()

	t.Run("default provider configured but current provider is not default", func(t *testing.T) {
		// When multiple providers are defined, default provider is specified and the current provider is
		// not default, then current provider should be added to the generated OpenID Connect callback URL.
		resp := rt.SendAdminRequest(http.MethodGet, "/db/_oidc?provider=facebook&offline=true", "")
		require.Equal(t, http.StatusFound, resp.Code)
		location := resp.Header().Get(headerLocation)
		require.NotEmpty(t, location, "Location should be available in response header")
		locationURL, err := url.Parse(location)
		require.NoError(t, err, "Location header should be a valid URL")
		redirectURI := locationURL.Query().Get(requestParamRedirectURI)
		require.NotEmpty(t, location, "redirect_uri should be available in auth URL")
		redirectURL, err := url.Parse(redirectURI)
		require.NoError(t, err, "redirect_uri should be a valid URL")
		assert.Equal(t, providerFacebook.Name, redirectURL.Query().Get(authProvider))
	})

	t.Run("default provider configured and current provider is default", func(t *testing.T) {
		// When multiple providers are defined, default provider is specified and the current provider is
		// default, then current provider should NOT be added to the generated OpenID Connect callback URL.
		resp := rt.SendAdminRequest(http.MethodGet, "/db/_oidc?provider=google&offline=true", "")
		require.Equal(t, http.StatusFound, resp.Code)
		location := resp.Header().Get(headerLocation)
		require.NotEmpty(t, location, "Location should be available in response header")
		locationURL, err := url.Parse(location)
		require.NoError(t, err, "Location header should be a valid URL")
		redirectURI := locationURL.Query().Get(requestParamRedirectURI)
		require.NotEmpty(t, location, "redirect_uri should be available in auth URL")
		redirectURL, err := url.Parse(redirectURI)
		require.NoError(t, err, "redirect_uri should be a valid URL")
		assert.Equal(t, "", redirectURL.Query().Get(authProvider))
	})

	t.Run("default provider configured but no current provider", func(t *testing.T) {
		// When multiple providers are defined, default provider is specified and no current provider is
		// provided, then provider name should NOT be added to the generated OpenID Connect callback URL.
		resp := rt.SendAdminRequest(http.MethodGet, "/db/_oidc?offline=true", "")
		require.Equal(t, http.StatusFound, resp.Code)
		location := resp.Header().Get(headerLocation)
		require.NotEmpty(t, location, "Location should be available in response header")
		locationURL, err := url.Parse(location)
		require.NoError(t, err, "Location header should be a valid URL")
		redirectURI := locationURL.Query().Get(requestParamRedirectURI)
		require.NotEmpty(t, location, "redirect_uri should be available in auth URL")
		redirectURL, err := url.Parse(redirectURI)
		require.NoError(t, err, "redirect_uri should be a valid URL")
		assert.Equal(t, "", redirectURL.Query().Get(authProvider))
	})
}

func TestCallbackState(t *testing.T) {
	authServer, err := mockAuthServer()
	require.NoError(t, err, "Error mocking fake authorization server")
	defer authServer.Close()
	issuerGoogle = authServer.URL + "/google"
	wantUsername := "google_noah"

	t.Run("check whether state is maintained when callback state is disabled explicitly", func(t *testing.T) {
		providerGoogle := auth.OIDCProvider{
			Name: providerNameGoogle, Issuer: issuerGoogle, ClientID: clientIDGoogle, IncludeAccessToken: true,
			UserPrefix: providerNameGoogle, ValidationKey: &validationKeyGoogle, Register: true,
			DiscoveryURI: issuerGoogle + wellKnownPath, DisableCallbackState: base.BoolPtr(true),
		}
		providers := auth.OIDCProviderMap{providerGoogle.Name: &providerGoogle}
		options := auth.OIDCOptions{Providers: providers, DefaultProvider: &providerGoogle.Name}
		restTesterConfig := RestTesterConfig{DatabaseConfig: &DbConfig{OIDCConfig: &options}}
		restTester := NewRestTester(t, &restTesterConfig)
		defer restTester.Close()
		fakeSyncGateway := httptest.NewServer(restTester.TestPublicHandler())
		defer fakeSyncGateway.Close()
		mockSyncGatewayURL = fakeSyncGateway.URL

		// Initiate OpenID Connect Authorization Code flow.
		requestURL := fmt.Sprintf("%s/db/_oidc?provider=google&offline=true", mockSyncGatewayURL)
		request, err := http.NewRequest(http.MethodGet, requestURL, nil)
		require.NoError(t, err, "Error creating new request")
		response, err := http.DefaultClient.Do(request)
		require.NoError(t, err, "Error sending request")
		require.Equal(t, http.StatusOK, response.StatusCode)

		// Validate received token response
		var receivedToken OIDCTokenResponse
		require.NoError(t, err, json.NewDecoder(response.Body).Decode(&receivedToken))
		assert.NotEmpty(t, receivedToken.SessionID, "session_id doesn't exist")
		assert.Equal(t, wantUsername, receivedToken.Username, "name mismatch")
		assert.Equal(t, wantTokenResponse.IDToken, receivedToken.IDToken, "id_token mismatch")
		assert.Equal(t, wantTokenResponse.RefreshToken, receivedToken.RefreshToken, "refresh_token mismatch")
		assert.Equal(t, wantTokenResponse.AccessToken, receivedToken.AccessToken, "access_token mismatch")
		assert.Equal(t, wantTokenResponse.TokenType, receivedToken.TokenType, "token_type mismatch")
		assert.True(t, wantTokenResponse.Expires >= receivedToken.Expires, "expires_in mismatch")

		// Query db endpoint with bearer token
		var responseBody db.Body
		dbEndpoint := mockSyncGatewayURL + "/" + restTester.DatabaseConfig.Name
		request, err = http.NewRequest(http.MethodGet, dbEndpoint, nil)
		request.Header.Add("Authorization", receivedToken.IDToken)
		response, err = http.DefaultClient.Do(request)
		require.NoError(t, err, "Error sending request with bearer token")
		require.Equal(t, http.StatusOK, response.StatusCode)
		require.NoError(t, err, json.NewDecoder(response.Body).Decode(&responseBody))
		assert.Equal(t, restTester.DatabaseConfig.Name, responseBody["db_name"])
	})

	t.Run("check whether state is maintained when callback state is enabled", func(t *testing.T) {
		providerGoogle := auth.OIDCProvider{
			Name: providerNameGoogle, Issuer: issuerGoogle, ClientID: clientIDGoogle, IncludeAccessToken: true,
			UserPrefix: providerNameGoogle, ValidationKey: &validationKeyGoogle, Register: true,
			DiscoveryURI: issuerGoogle + wellKnownPath, DisableCallbackState: base.BoolPtr(false),
		}
		providers := auth.OIDCProviderMap{providerGoogle.Name: &providerGoogle}
		options := auth.OIDCOptions{Providers: providers, DefaultProvider: &providerGoogle.Name}
		restTesterConfig := RestTesterConfig{DatabaseConfig: &DbConfig{OIDCConfig: &options}}
		restTester := NewRestTester(t, &restTesterConfig)
		defer restTester.Close()
		fakeSyncGateway := httptest.NewServer(restTester.TestPublicHandler())
		defer fakeSyncGateway.Close()
		mockSyncGatewayURL = fakeSyncGateway.URL

		// Initiate OpenID Connect Authorization Code flow.
		requestURL := fmt.Sprintf("%s/db/_oidc?provider=google&offline=true", mockSyncGatewayURL)
		request, err := http.NewRequest(http.MethodGet, requestURL, nil)
		require.NoError(t, err, "Error creating new request")
		jar, err := cookiejar.New(nil)
		require.NoError(t, err, "Error creating new cookie jar")
		client := &http.Client{Jar: jar}
		response, err := client.Do(request)
		require.NoError(t, err, "Error sending request")
		require.Equal(t, http.StatusOK, response.StatusCode)

		// Validate received token response
		var receivedToken OIDCTokenResponse
		require.NoError(t, err, json.NewDecoder(response.Body).Decode(&receivedToken))
		assert.NotEmpty(t, receivedToken.SessionID, "session_id doesn't exist")
		assert.Equal(t, wantUsername, receivedToken.Username, "name mismatch")
		assert.Equal(t, wantTokenResponse.IDToken, receivedToken.IDToken, "id_token mismatch")
		assert.Equal(t, wantTokenResponse.RefreshToken, receivedToken.RefreshToken, "refresh_token mismatch")
		assert.Equal(t, wantTokenResponse.AccessToken, receivedToken.AccessToken, "access_token mismatch")
		assert.Equal(t, wantTokenResponse.TokenType, receivedToken.TokenType, "token_type mismatch")
		assert.True(t, wantTokenResponse.Expires >= receivedToken.Expires, "expires_in mismatch")

		// Query db endpoint with bearer token
		var responseBody db.Body
		dbEndpoint := mockSyncGatewayURL + "/" + restTester.DatabaseConfig.Name
		request, err = http.NewRequest(http.MethodGet, dbEndpoint, nil)
		request.Header.Add("Authorization", receivedToken.IDToken)
		response, err = client.Do(request)
		require.NoError(t, err, "Error sending request with bearer token")
		require.Equal(t, http.StatusOK, response.StatusCode)
		require.NoError(t, err, json.NewDecoder(response.Body).Decode(&responseBody))
		assert.Equal(t, restTester.DatabaseConfig.Name, responseBody["db_name"])
	})

	t.Run("check whether state is maintained when callback state is disabled implicitly", func(t *testing.T) {
		providerGoogle := auth.OIDCProvider{
			Name: providerNameGoogle, Issuer: issuerGoogle, ClientID: clientIDGoogle,
			UserPrefix: providerNameGoogle, ValidationKey: &validationKeyGoogle, Register: true,
			DiscoveryURI: issuerGoogle + wellKnownPath, IncludeAccessToken: true,
		}
		providers := auth.OIDCProviderMap{providerGoogle.Name: &providerGoogle}
		options := auth.OIDCOptions{Providers: providers, DefaultProvider: &providerGoogle.Name}
		restTesterConfig := RestTesterConfig{DatabaseConfig: &DbConfig{OIDCConfig: &options}}
		restTester := NewRestTester(t, &restTesterConfig)
		defer restTester.Close()
		fakeSyncGateway := httptest.NewServer(restTester.TestPublicHandler())
		defer fakeSyncGateway.Close()
		mockSyncGatewayURL = fakeSyncGateway.URL

		// Initiate OpenID Connect Authorization Code flow.
		requestURL := fmt.Sprintf("%s/db/_oidc?provider=google&offline=true", mockSyncGatewayURL)
		request, err := http.NewRequest(http.MethodGet, requestURL, nil)
		require.NoError(t, err, "Error creating new request")
		jar, err := cookiejar.New(nil)
		require.NoError(t, err, "Error creating new cookie jar")
		client := &http.Client{Jar: jar}
		response, err := client.Do(request)
		require.NoError(t, err, "Error sending request")
		require.Equal(t, http.StatusOK, response.StatusCode)

		// Validate received token response
		var receivedToken OIDCTokenResponse
		require.NoError(t, err, json.NewDecoder(response.Body).Decode(&receivedToken))
		assert.NotEmpty(t, receivedToken.SessionID, "session_id doesn't exist")
		assert.Equal(t, wantUsername, receivedToken.Username, "name mismatch")
		assert.Equal(t, wantTokenResponse.IDToken, receivedToken.IDToken, "id_token mismatch")
		assert.Equal(t, wantTokenResponse.RefreshToken, receivedToken.RefreshToken, "refresh_token mismatch")
		assert.Equal(t, wantTokenResponse.AccessToken, receivedToken.AccessToken, "access_token mismatch")
		assert.Equal(t, wantTokenResponse.TokenType, receivedToken.TokenType, "token_type mismatch")
		assert.True(t, wantTokenResponse.Expires >= receivedToken.Expires, "expires_in mismatch")

		// Query db endpoint with bearer token
		var responseBody db.Body
		dbEndpoint := mockSyncGatewayURL + "/" + restTester.DatabaseConfig.Name
		request, err = http.NewRequest(http.MethodGet, dbEndpoint, nil)
		request.Header.Add("Authorization", receivedToken.IDToken)
		response, err = client.Do(request)
		require.NoError(t, err, "Error sending request with bearer token")
		require.Equal(t, http.StatusOK, response.StatusCode)
		require.NoError(t, err, json.NewDecoder(response.Body).Decode(&responseBody))
		assert.Equal(t, restTester.DatabaseConfig.Name, responseBody["db_name"])
	})
}
