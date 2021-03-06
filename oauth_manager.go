package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	osin "github.com/lonelycode/osin"
	"github.com/nu7hatch/gouuid"
	"net/http"
	"time"
)

/*

Sample Oaut Flow:
-----------------

1. Request to /authorize
2. Tyk extracts all relevant data and pre-screens client_id, client_secret and redirect_uri
3. Instead of proxying the request it redirects the user to the login page on the resource with the client_id & secret as a POST (basically passed through)
4. Resource presents approve / deny window to user
5. If approve is clicked, resource pings oauth/authorise which is the actual authorize endpoint (requires admin key),
   this returns oauth details to resource as well as redirect URI which it can then redirec to
6. User is redirected to redirect URI with auth_code
7. Client makes auth request for bearer token
8. Client API makes all calls with bearer token

Effort required by Resource Owner:
1. Create a login & approve/deny page
2. Send an API request to Tyk to generate an auth_code
3. Create endpoint to accept key change notifications

*/

// OAuthClient is a representation within an APISpec of a client
type OAuthClient struct {
	ClientID          string `json:"client_id"`
	ClientSecret      string `json:"secret"`
	ClientRedirectURI string `json:"redirect_uri"`
}

// OAuthNotificationType const to reduce risk of colisions
type OAuthNotificationType string

// Notifcation codes for new and refresh codes
const (
	NEW_ACCESS_TOKEN     OAuthNotificationType = "new"
	REFRESH_ACCESS_TOKEN OAuthNotificationType = "refresh"
)

// NewOAuthNotification is a notification sent to a
// webhook when an access request or a refresh request comes in.
type NewOAuthNotification struct {
	AuthCode         string                `json:"auth_code"`
	NewOAuthToken    string                `json:"new_oauth_token"`
	RefreshToken     string                `json:"refresh_token"`
	OldRefreshToken  string                `json:"old_refresh_token"`
	NotificationType OAuthNotificationType `json:"notification_type"`
}

// OAuthHandlers are the HTTP Handlers that manage the Tyk OAuth flow
type OAuthHandlers struct {
	Manager OAuthManager
}

func (o *OAuthHandlers) generateOAuthOutputFromOsinResponse(osinResponse *osin.Response) ([]byte, bool) {

	// TODO: Might need to clear this out
	if osinResponse.Output["state"] == "" {
		log.Debug("Removing state")
		delete(osinResponse.Output, "state")
	}

	redirect, rediErr := osinResponse.GetRedirectUrl()

	if rediErr == nil {
		// Hack to inject redirect into response
		osinResponse.Output["redirect_to"] = redirect
	}

	if respData, marshalErr := json.Marshal(&osinResponse.Output); marshalErr != nil {
		return []byte{}, false
	} else {
		return respData, true
	}

}

func (o *OAuthHandlers) notifyClientOfNewOauth(notification NewOAuthNotification) bool {
	log.Info("Notifying client host")
	go o.Manager.API.NotificationsDetails.SendRequest(false, 0, notification)
	return true
}

// HandleGenerateAuthCodeData handles a resource provider approving an OAuth request from a client
func (o *OAuthHandlers) HandleGenerateAuthCodeData(w http.ResponseWriter, r *http.Request) {
	var responseMessage []byte
	var code int

	if r.Method == "POST" {
		// On AUTH grab session state data and add to UserData (not validated, not good!)
		sessionStateJSONData := r.FormValue("key_rules")
		if sessionStateJSONData == "" {
			responseMessage := createError("Authorise request is missing key_rules in params")
			w.WriteHeader(400)
			fmt.Fprintf(w, string(responseMessage))
			return
		}

		// Handle the authorisation and write the JSON output to the resource provider
		resp := o.Manager.HandleAuthorisation(r, true, sessionStateJSONData)
		code = 200
		responseMessage, _ = o.generateOAuthOutputFromOsinResponse(resp)
		if resp.IsError {
			code = resp.ErrorStatusCode
			log.Error("OAuth response marked as error")
			log.Error(resp)
		}

	} else {
		// Return Not supported message (and code)
		code = 405
		responseMessage = createError("Method not supported")
	}

	w.WriteHeader(code)
	fmt.Fprintf(w, string(responseMessage))
}

// HandleAuthorizePassthrough handles a Client Auth request, first it checks if the client
// is OK (otherwise it blocks the request), then it forwards on to the resource providers approval URI
func (o *OAuthHandlers) HandleAuthorizePassthrough(w http.ResponseWriter, r *http.Request) {
	var responseMessage []byte
	var code int

	if r.Method == "GET" || r.Method == "POST" {
		// Extract client data and check
		resp := o.Manager.HandleAuthorisation(r, false, "")
		responseMessage, _ = o.generateOAuthOutputFromOsinResponse(resp)
		if resp.IsError {
			log.Error("There was an error with the request")
			log.Error(resp)
			// Something went wrong, write out the error details and kill the response
			w.WriteHeader(resp.ErrorStatusCode)
			responseMessage = createError(resp.StatusText)
			fmt.Fprintf(w, string(responseMessage))
			return
		}

		w.Header().Add("Location", o.Manager.API.Oauth2Meta.AuthorizeLoginRedirect)
		w.WriteHeader(307)

	} else {
		// Return Not supported message (and code)
		code = 405
		responseMessage = createError("Method not supported")
		w.WriteHeader(code)
		fmt.Fprintf(w, string(responseMessage))
	}

}

// HandleAccessRequest handles the OAuth 2.0 token or refresh access request, and wraps Tyk's own and Osin's OAuth handlers,
// returns a response to the client and notifies the provider of the access request (in order to track identity against
// OAuth tokens without revealing tokens before they are requested).
func (o *OAuthHandlers) HandleAccessRequest(w http.ResponseWriter, r *http.Request) {
	var responseMessage []byte
	var code int

	if r.Method == "GET" || r.Method == "POST" {
		// Handle response
		resp := o.Manager.HandleAccess(r)
		responseMessage, _ = o.generateOAuthOutputFromOsinResponse(resp)
		if resp.IsError {
			// Something went wrong, write out the error details and kill the response
			w.WriteHeader(resp.ErrorStatusCode)
			fmt.Fprintf(w, string(responseMessage))
			return
		}

		// Ping endpoint with o_auth key and auth_key
		code = 200
		code := r.FormValue("code")
		OldRefreshToken := r.FormValue("refresh_token")
		log.Debug("AUTH CODE: ", code)
		NewOAuthToken := ""
		if resp.Output["access_token"] != nil {
			NewOAuthToken = resp.Output["access_token"].(string)
		}
		log.Debug("TOKEN: ", NewOAuthToken)
		RefreshToken := ""
		if resp.Output["refresh_token"] != nil {
			RefreshToken = resp.Output["refresh_token"].(string)
		}
		log.Debug("REFRESH: ", RefreshToken)
		log.Debug("Old REFRESH: ", OldRefreshToken)

		notificationType := NEW_ACCESS_TOKEN
		if OldRefreshToken != "" {
			notificationType = REFRESH_ACCESS_TOKEN
		}

		newNotification := NewOAuthNotification{
			AuthCode:         code,
			NewOAuthToken:    NewOAuthToken,
			RefreshToken:     RefreshToken,
			OldRefreshToken:  OldRefreshToken,
			NotificationType: notificationType,
		}

		o.notifyClientOfNewOauth(newNotification)

	} else {
		// Return Not supported message (and code)
		code = 405
		responseMessage = createError("Method not supported")
	}

	w.WriteHeader(code)
	fmt.Fprintf(w, string(responseMessage))
}

// OAuthManager handles and wraps osin OAuth2 functions to handle authorise and access requests
type OAuthManager struct {
	API        *APISpec
	OsinServer *TykOsinServer
}

// HandleAuthorisation creates the authorisation data for the request
func (o *OAuthManager) HandleAuthorisation(r *http.Request, complete bool, sessionState string) *osin.Response {
	resp := o.OsinServer.NewResponse()

	if ar := o.OsinServer.HandleAuthorizeRequest(resp, r); ar != nil {
		// Since this is called by the Reource provider (proxied API), we assume it has been approved
		ar.Authorized = true

		if complete {
			ar.UserData = sessionState
			o.OsinServer.FinishAuthorizeRequest(resp, r, ar)
		}
	}
	if resp.IsError && resp.InternalError != nil {
		fmt.Printf("ERROR: %s\n", resp.InternalError)
	}

	return resp
}

// HandleAccess wraps an access request with osin's primitives
func (o *OAuthManager) HandleAccess(r *http.Request) *osin.Response {
	resp := o.OsinServer.NewResponse()
	if ar := o.OsinServer.HandleAccessRequest(resp, r); ar != nil {
		ar.Authorized = true
		o.OsinServer.FinishAccessRequest(resp, r, ar)
	}
	if resp.IsError && resp.InternalError != nil {
		log.Error("ERROR: ", resp.InternalError)
	}

	return resp
}

// These enums fix the prefix to use when storing various OAuth keys and data, since we
// delegate everything to the osin framework
const (
	AUTH_PREFIX    string = "oauth-authorize."
	CLIENT_PREFIX  string = "oauth-clientid."
	ACCESS_PREFIX  string = "oauth-access."
	REFRESH_PREFIX string = "oauth-refresh."
)

type ExtendedOsinStorageInterface interface {
	// Create OAuth clients
	SetClient(id string, client osin.Client, ignorePrefix bool) error

	// Custom getter to handle prefixing issues in Redis
	GetClientNoPrefix(id string) (osin.Client, error)

	GetClients(filter string, ignorePrefix bool) ([]osin.Client, error)

	DeleteClient(id string, ignorePrefix bool) error

	// Clone the storage if needed. For example, using mgo, you can clone the session with session.Clone
	// to avoid concurrent access problems.
	// This is to avoid cloning the connection at each method access.
	// Can return itself if not a problem.
	Clone() osin.Storage

	// Close the resources the Storate potentially holds (using Clone for example)
	Close()

	// GetClient loads the client by id (client_id)
	GetClient(id string) (osin.Client, error)

	// SaveAuthorize saves authorize data.
	SaveAuthorize(*osin.AuthorizeData) error

	// LoadAuthorize looks up AuthorizeData by a code.
	// Client information MUST be loaded together.
	// Optionally can return error if expired.
	LoadAuthorize(code string) (*osin.AuthorizeData, error)

	// RemoveAuthorize revokes or deletes the authorization code.
	RemoveAuthorize(code string) error

	// SaveAccess writes AccessData.
	// If RefreshToken is not blank, it must save in a way that can be loaded using LoadRefresh.
	SaveAccess(*osin.AccessData) error

	// LoadAccess retrieves access data by token. Client information MUST be loaded together.
	// AuthorizeData and AccessData DON'T NEED to be loaded if not easily available.
	// Optionally can return error if expired.
	LoadAccess(token string) (*osin.AccessData, error)

	// RemoveAccess revokes or deletes an AccessData.
	RemoveAccess(token string) error

	// LoadRefresh retrieves refresh AccessData. Client information MUST be loaded together.
	// AuthorizeData and AccessData DON'T NEED to be loaded if not easily available.
	// Optionally can return error if expired.
	LoadRefresh(token string) (*osin.AccessData, error)

	// RemoveRefresh revokes or deletes refresh AccessData.
	RemoveRefresh(token string) error
}

// TykOsinServer subclasses osin.Server so we can add the SetClient method without wrecking the lbrary
type TykOsinServer struct {
	osin.Server
	Config            *osin.ServerConfig
	Storage           ExtendedOsinStorageInterface
	AuthorizeTokenGen osin.AuthorizeTokenGen
	AccessTokenGen    osin.AccessTokenGen
}

// TykOsinNewServer creates a new server instance, but uses an extended interface so we can SetClient() too.
func TykOsinNewServer(config *osin.ServerConfig, storage ExtendedOsinStorageInterface) *TykOsinServer {

	overrideServer := TykOsinServer{
		Config:            config,
		Storage:           storage,
		AuthorizeTokenGen: &osin.AuthorizeTokenGenDefault{},
		AccessTokenGen:    &osin.AccessTokenGenDefault{},
	}

	overrideServer.Server.Config = config
	overrideServer.Server.Storage = storage
	overrideServer.Server.AuthorizeTokenGen = overrideServer.AuthorizeTokenGen
	overrideServer.Server.AccessTokenGen = overrideServer.AccessTokenGen

	return &overrideServer
}

// TODO: Refactor this to move prefix handling into a checker method, then it can be an unexported setting in the struct.
// RedisOsinStorageInterface implements osin.Storage interface to use Tyk's own storage mechanism
type RedisOsinStorageInterface struct {
	store          StorageHandler
	sessionManager SessionHandler
}

func (r RedisOsinStorageInterface) Clone() osin.Storage {
	return r
}

func (r RedisOsinStorageInterface) Close() {}

// GetClient will retrieve client data
func (r RedisOsinStorageInterface) GetClient(id string) (osin.Client, error) {
	key := CLIENT_PREFIX + id

	clientJSON, storeErr := r.store.GetKey(key)

	if storeErr != nil {
		log.Error("Failure retreiving client ID key")
		log.Error(storeErr)
		return nil, storeErr
	}

	thisClient := new(osin.DefaultClient)
	if marshalErr := json.Unmarshal([]byte(clientJSON), &thisClient); marshalErr != nil {
		log.Error("Couldn't unmarshal OAuth client object")
		log.Error(marshalErr)
	}

	return thisClient, nil
}

// GetClientNoPrefix will retrieve client data, but not asign a prefix - this is an unfortunate hack,
// but we don't want to change the signature in Osin for GetClient to support the odd Redis prefixing
func (r RedisOsinStorageInterface) GetClientNoPrefix(id string) (osin.Client, error) {

	key := id

	clientJSON, storeErr := r.store.GetKey(key)

	if storeErr != nil {
		log.Error("Failure retreiving client ID key")
		log.Error(storeErr)
		return nil, storeErr
	}

	thisClient := new(osin.DefaultClient)
	if marshalErr := json.Unmarshal([]byte(clientJSON), &thisClient); marshalErr != nil {
		log.Error("Couldn't unmarshal OAuth client object")
		log.Error(marshalErr)
	}

	return thisClient, nil
}

// GetClients will retreive a list of clients for a prefix
func (r RedisOsinStorageInterface) GetClients(filter string, ignorePrefix bool) ([]osin.Client, error) {
	key := CLIENT_PREFIX + filter
	if ignorePrefix {
		key = filter
	}

	clientJSON := r.store.GetKeysAndValuesWithFilter(key)

	theseClients := []osin.Client{}

	for _, clientJSON := range clientJSON {
		thisClient := new(osin.DefaultClient)
		if marshalErr := json.Unmarshal([]byte(clientJSON), &thisClient); marshalErr != nil {
			log.Error("Couldn't unmarshal OAuth client object")
			log.Error(marshalErr)
			return theseClients, marshalErr
		}
		theseClients = append(theseClients, thisClient)
	}

	return theseClients, nil
}

// SetClient creates client data
func (r RedisOsinStorageInterface) SetClient(id string, client osin.Client, ignorePrefix bool) error {
	clientDataJSON, err := json.Marshal(client)

	if err != nil {
		log.Error("Couldn't marshal client data")
		log.Error(err)
		return err
	}

	key := CLIENT_PREFIX + id
	
	if ignorePrefix {
		key = id
	}
	
	log.Warning("CREATING: ", key)
	
	r.store.SetKey(key, string(clientDataJSON), 0)
	return nil
}

// DeleteClient Removes a client from the system
func (r RedisOsinStorageInterface) DeleteClient(id string, ignorePrefix bool) error {
	key := CLIENT_PREFIX + id
	if ignorePrefix {
		key = id
	}

	r.store.DeleteKey(key)
	return nil
}

// SaveAuthorize saves authorisation data to REdis
func (r RedisOsinStorageInterface) SaveAuthorize(authData *osin.AuthorizeData) error {
	if authDataJSON, marshalErr := json.Marshal(&authData); marshalErr != nil {
		return marshalErr
	} else {
		key := AUTH_PREFIX + authData.Code
		log.Debug("Saving auth code: ", key)
		r.store.SetKey(key, string(authDataJSON), int64(authData.ExpiresIn))
		return nil

	}

}

// LoadAuthorize loads auth data from redis
func (r RedisOsinStorageInterface) LoadAuthorize(code string) (*osin.AuthorizeData, error) {
	key := AUTH_PREFIX + code
	log.Debug("Loading auth code: ", key)
	authJSON, storeErr := r.store.GetKey(key)

	if storeErr != nil {
		log.Error("Failure retreiving auth code key")
		log.Error(storeErr)
		return nil, storeErr
	}

	thisAuthData := osin.AuthorizeData{}
	thisAuthData.Client = new(osin.DefaultClient)
	if marshalErr := json.Unmarshal([]byte(authJSON), &thisAuthData); marshalErr != nil {
		log.Error("Couldn't unmarshal OAuth auth data object (LoadAuthorize)")
		log.Error(marshalErr)
		return nil, marshalErr
	}

	return &thisAuthData, nil
}

// RemoveAuthorize removes authorisation keys from redis
func (r RedisOsinStorageInterface) RemoveAuthorize(code string) error {
	key := AUTH_PREFIX + code
	r.store.DeleteKey(key)
	return nil
}

// SaveAccess will save a token and it's access data to redis
func (r RedisOsinStorageInterface) SaveAccess(accessData *osin.AccessData) error {
	authDataJSON, marshalErr := json.Marshal(accessData)
	if marshalErr != nil {
		return marshalErr
	}

	key := ACCESS_PREFIX + accessData.AccessToken
	log.Debug("Saving ACCESS key: ", key)
	r.store.SetKey(key, string(authDataJSON), int64(accessData.ExpiresIn))

	// Create a SessionState object and register it with the authmanager
	var newSession SessionState
	unmarshalErr := json.Unmarshal([]byte(accessData.UserData.(string)), &newSession)

	if unmarshalErr != nil {
		log.Error("Couldn't decode SessionState from UserData")
		log.Error(unmarshalErr)
		return unmarshalErr
	}

	// Set the client ID for analytics
	newSession.OauthClientID = accessData.Client.GetId()

	// Override timeouts so that we can be in sync with Osin
	newSession.Expires = time.Now().Unix() + int64(accessData.ExpiresIn)

	// Use the default session expiry here as this is OAuth
	r.sessionManager.UpdateSession(accessData.AccessToken, newSession, newSession.Expires)

	// Store the refresh token too
	if accessData.RefreshToken != "" {
		if accessDataJSON, marshalErr := json.Marshal(&accessData); marshalErr != nil {
			return marshalErr
		} else {
			key := REFRESH_PREFIX + accessData.RefreshToken
			log.Debug("Saving REFRESH key: ", key)
			r.store.SetKey(key, string(accessDataJSON), int64(accessData.ExpiresIn))
			return nil
		}

	}

	return nil
}

// LoadAccess will load access data from redis
func (r RedisOsinStorageInterface) LoadAccess(token string) (*osin.AccessData, error) {
	key := ACCESS_PREFIX + token
	log.Debug("Loading ACCESS key: ", key)
	accessJSON, storeErr := r.store.GetKey(key)

	if storeErr != nil {
		log.Error("Failure retreiving access token by key")
		log.Error(storeErr)
		return nil, storeErr
	}

	thisAccessData := osin.AccessData{}
	thisAccessData.Client = new(osin.DefaultClient)
	if marshalErr := json.Unmarshal([]byte(accessJSON), &thisAccessData); marshalErr != nil {
		log.Error("Couldn't unmarshal OAuth auth data object (LoadAccess)")
		log.Error(marshalErr)
		return nil, marshalErr
	}

	return &thisAccessData, nil
}

// RemoveAccess will remove access data from Redis
func (r RedisOsinStorageInterface) RemoveAccess(token string) error {
	key := ACCESS_PREFIX + token
	r.store.DeleteKey(key)

	// remove the access token from central storage too
	r.sessionManager.RemoveSession(token)

	return nil
}

// LoadRefresh will load access data from Redis
func (r RedisOsinStorageInterface) LoadRefresh(token string) (*osin.AccessData, error) {
	key := REFRESH_PREFIX + token
	log.Debug("Loading REFRESH key: ", key)
	accessJSON, storeErr := r.store.GetKey(key)

	if storeErr != nil {
		log.Error("Failure retreiving access token by key")
		log.Error(storeErr)
		return nil, storeErr
	}

	// new interface means having to make this nested... ick.
	thisAccessData := osin.AccessData{}
	thisAccessData.Client = new(osin.DefaultClient)
	thisAccessData.AuthorizeData = &osin.AuthorizeData{}
	thisAccessData.AuthorizeData.Client = new(osin.DefaultClient)

	if marshalErr := json.Unmarshal([]byte(accessJSON), &thisAccessData); marshalErr != nil {
		log.Error("Couldn't unmarshal OAuth auth data object (LoadRefresh)")
		log.Error(marshalErr)
		return nil, marshalErr
	}

	return &thisAccessData, nil
}

// RemoveRefresh will remove a refresh token from redis
func (r RedisOsinStorageInterface) RemoveRefresh(token string) error {
	key := REFRESH_PREFIX + token
	r.store.DeleteKey(key)
	return nil
}

// AccessTokenGenTyk is a modified authorization token generator that uses the same method used to generate tokens for Tyk authHandler
type AccessTokenGenTyk struct {
	sessionManager SessionHandler
}

// GenerateAccessToken generates base64-encoded UUID access and refresh tokens
func (a *AccessTokenGenTyk) GenerateAccessToken(data *osin.AccessData, generaterefresh bool) (accesstoken string, refreshtoken string, err error) {
	log.Info("Generating new token")

	var newSession SessionState
	marshalErr := json.Unmarshal([]byte(data.UserData.(string)), &newSession)

	if marshalErr != nil {
		log.Error("Couldn't decode SessionState from UserData")
		log.Error(marshalErr)
		return "", "", marshalErr
	}

	accesstoken = keyGen.GenerateAuthKey(newSession.OrgID)

	if generaterefresh {
		u6, _ := uuid.NewV4()
		refreshtoken = base64.StdEncoding.EncodeToString([]byte(u6.String()))
	}
	return
}
